package dkafka

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	pbcodec "github.com/dfuse-io/dfuse-eosio/pb/dfuse/eosio/codec/v1"
	"github.com/google/cel-go/cel"
	"go.uber.org/zap"
)

type SaveBlock = func(*pbcodec.Block)
type DecodeDBOps = func(in []*pbcodec.DBOp, blockNum uint32) (decodedDBOps []*decodedDBOp, err error)

func saveBlockNoop(*pbcodec.Block) {
	// does nothing
}

func saveBlockJSON(block *pbcodec.Block) {
	byteArray, err := json.Marshal(block)
	if err != nil {
		zlog.Error("Fail to marshal to JSON incoming block", zap.Uint32("id", block.Number), zap.Error(err))
	}
	// the WriteFile method returns an error if unsuccessful
	fileName := fmt.Sprintf("block-%d.json", block.Number)
	err = ioutil.WriteFile(fileName, byteArray, 0644)
	// handle this error
	if err != nil {
		zlog.Error("Fail to write file", zap.String("file", fileName), zap.Error(err))
	}
}

type adapter struct {
	topic                 string
	saveBlock             SaveBlock
	decodeDBOps           DecodeDBOps
	failOnUndecodableDBOP bool
	eventTypeProg         cel.Program
	eventKeyProg          cel.Program
	extensions            []*extension
	// TODO merge all headers
	headers []kafka.Header
}

func newAdapter(
	topic string,
	saveBlock SaveBlock,
	decodeDBOps DecodeDBOps,
	failOnUndecodableDBOP bool,
	eventTypeProg cel.Program,
	eventKeyProg cel.Program,
	extensions []*extension,
	headers []kafka.Header,
) adapter {
	return adapter{topic, saveBlock, decodeDBOps, failOnUndecodableDBOP, eventTypeProg, eventKeyProg, extensions, headers}
}

func (m *adapter) adapt(blk *pbcodec.Block, rawStep string) (*kafka.Message, error) {
	m.saveBlock(blk)
	step := sanitizeStep(rawStep)

	if blk.Number%100 == 0 {
		zlog.Info("incoming block 1/100", zap.Uint32("blk_number", blk.Number), zap.String("step", step), zap.Int("length_filtered_trx_traces", len(blk.FilteredTransactionTraces)))
	}
	if blk.Number%10 == 0 {
		zlog.Debug("incoming block 1/10", zap.Uint32("blk_number", blk.Number), zap.String("step", step), zap.Int("length_filtered_trx_traces", len(blk.FilteredTransactionTraces)))
	}

	for _, trx := range blk.TransactionTraces() {
		transactionTracesReceived.Inc()
		status := sanitizeStatus(trx.Receipt.Status.String())
		// manage correlation
		correlation, err := getCorrelation(trx.ActionTraces)
		if err != nil {
			return nil, err
		}
		for _, act := range trx.ActionTraces {
			if !act.FilteringMatched {
				continue
			}
			actionTracesReceived.Inc()
			var jsonData json.RawMessage
			if act.Action.JsonData != "" {
				jsonData = json.RawMessage(act.Action.JsonData)
			}

			var authorizations []string
			for _, auth := range act.Action.Authorization {
				authorizations = append(authorizations, auth.Authorization())
			}

			var globalSeq uint64
			if act.Receipt != nil {
				globalSeq = act.Receipt.GlobalSequence
			}

			decodedDBOps, err := m.decodeDBOps(trx.DBOpsForAction(act.ExecutionIndex), blk.Number)
			if err != nil {
				if m.failOnUndecodableDBOP {
					return nil, err
				}
				zlog.Warn("cannot decode dbops", zap.Uint32("block_number", blk.Number), zap.Error(err))
			}
			// memoizableTrxTrace := &filtering.MemoizableTrxTrace{TrxTrace: trx}
			// activation := filtering.NewActionTraceActivation(
			// 	act,
			// 	memoizableTrxTrace,
			// 	rawStep,
			// )

			activation, err := NewActivation(step, trx,
				act,
				decodedDBOps,
			)
			if err != nil {
				return nil, err
			}

			eosioAction := event{
				BlockNum:      blk.Number,
				BlockID:       blk.Id,
				Status:        status,
				Executed:      !trx.HasBeenReverted(),
				Step:          step,
				Correlation:   correlation,
				TransactionID: trx.Id,
				ActionInfo: ActionInfo{
					Account:        act.Account(),
					Receiver:       act.Receiver,
					Action:         act.Name(),
					JSONData:       &jsonData,
					DBOps:          decodedDBOps,
					Authorization:  authorizations,
					GlobalSequence: globalSeq,
				},
			}

			eventType, err := evalString(m.eventTypeProg, activation)
			if err != nil {
				return nil, fmt.Errorf("error eventtype eval: %w", err)
			}

			extensionsKV := make(map[string]string)
			for _, ext := range m.extensions {
				val, err := evalString(ext.prog, activation)
				if err != nil {
					return nil, fmt.Errorf("program: %w", err)
				}
				extensionsKV[ext.name] = val

			}

			eventKeys, err := evalStringArray(m.eventKeyProg, activation)
			if err != nil {
				return nil, fmt.Errorf("event keyeval: %w", err)
			}

			dedupeMap := make(map[string]bool)
			for _, eventKey := range eventKeys {
				if dedupeMap[eventKey] {
					continue
				}
				dedupeMap[eventKey] = true

				headers := append(m.headers,
					kafka.Header{
						Key:   "ce_id",
						Value: hashString(fmt.Sprintf("%s%s%d%s%s", blk.Id, trx.Id, act.ExecutionIndex, rawStep, eventKey)),
					},
					kafka.Header{
						Key:   "ce_type",
						Value: []byte(eventType),
					},
					kafka.Header{
						Key:   "ce_time",
						Value: []byte(blk.MustTime().Format("2006-01-02T15:04:05.9Z")),
					},
					kafka.Header{
						Key:   "ce_blkstep",
						Value: []byte(step),
					},
				)
				for k, v := range extensionsKV {
					headers = append(headers, kafka.Header{
						Key:   k,
						Value: []byte(v),
					})
				}
				msg := &kafka.Message{
					Key:     []byte(eventKey),
					Headers: headers,
					Value:   eosioAction.JSON(),
					TopicPartition: kafka.TopicPartition{
						Topic:     &m.topic,
						Partition: kafka.PartitionAny,
					},
				}
				return msg, nil
			}
		}
	}
	return nil, nil
}

func hashString(data string) []byte {
	h := sha256.New()
	h.Write([]byte(data))
	return []byte(base64.StdEncoding.EncodeToString(([]byte(h.Sum(nil)))))
}

func sanitizeStep(step string) string {
	return strings.Title(strings.TrimPrefix(step, "STEP_"))
}
func sanitizeStatus(status string) string {
	return strings.Title(strings.TrimPrefix(status, "TRANSACTIONSTATUS_"))
}
