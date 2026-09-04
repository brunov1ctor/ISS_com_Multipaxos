// Copyright 2022 IBM Corp. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"
	"gopkg.in/yaml.v2"

	"fmt"
	"hash/crc32"
	"io/ioutil"
	"strconv"
	"strings"
	"time"
)

var Config configuration

type configuration struct {
	LoggingLevelStr string `yaml:"Logging"`
	LoggingLevel    zerolog.Level

	UseTLS bool `yaml:"UseTLS"` // Use TLS for both peer-to-peer and client-to-peer communication.

	// The 3 options below are ignored if UseTLS is set to false.
	CACertFile string `yaml:"CACertFile"`
	KeyFile    string `yaml:"KeyFile"`
	CertFile   string `yaml:"CertFile"`

	// Network connections
	BasicConnections    int `yaml:"BasicConnections"`    // Number underlying parallel network connections for each logical connection between 2 peers.
	PriorityConnections int `yaml:"PriorityConnections"` // Number of parallel priority connections between 2 peers

	TestConnections       bool `yaml:"TestConnections"`       // Enable testing of connections before the actual experiment starts.
	ExcessConnections     int  `yaml:"ExcessConnections"`     // Number of extra connections to open when choosing the fastest ones. Those connections will be closed after the test.
	ConnectionTestMsgs    int  `yaml:"ConnectionTestMsgs"`    // Number of messages to use for testing a single connection.
	ConnectionTestPayload int  `yaml:"ConnectionTestPayload"` // Number of bytes in the payload of each connection test message.
	OutMessageBufSize     int  `yaml:"OutMessageBufsize"`     // Buffer size of channels used for outgoing messages. If 0, no channels are used.
	OutMessageBatchPeriod int  `yaml:"OutMessageBatchPeriod"` // Batching period of outgoing non-priority messages to each peer.
	ThroughputCap         int  `yaml:"ThroughputCap"`         // Batches are not cut faster than at this rate (system-wide).
	StragglerTolerance    int  `yaml:"StragglerTolerance"`
	BatchSizeIncrement    int  `yaml:"BatchSizeIncrement"`

	// Startup config
	Orderer           string `yaml:"Orderer"`
	Manager           string `yaml:"Manager"`
	Checkpointer      string `yaml:"Checkpointer"`
	Failures          int    `yaml:"Failures"`
	CrashTiming       string `yaml:"CrashTiming"`
	RandomSeed        int64  `yaml:"RandomSeed"`
	NodeToLeaderRatio int    `yaml:"NodeToLeaderRatio"`

	// Dummy Manager Config
	CheckpointInterval  int `yaml:"CheckpointInterval"`  // The checkpointing protocol is triggered each checkpointInterval of contiguously committed sequence numbers.
	WatermarkWindowSize int `yaml:"WatermarkWindowSize"` // The number of "in-flight" sequence numbers.
	// I.e., the maximum difference between the first uncommitted sequence number and the last sequence number for which
	// a value can be proposed, plus 1.

	// Mir Manager Config
	EpochLength        int  `yaml:"EpochLength"`
	SegmentLength      int  `yaml:"SegmentLength"`
	WaitForCheckpoints bool `yaml:"WaitForCheckpoints"`

	// Request Buffer Config
	ClientWatermarkWindowSize int `yaml:"ClientWatermarkWindowSize"`
	ClientRequestBacklogSize  int `yaml:"ClientRequestBacklogSize"` // The number of requests beyond client's current window that are backlogged.

	// Manager config
	LeaderPolicy     string `yaml:"LeaderPolicy"`
	DefaultLeaderBan int    `yaml:"DefaultLeaderBan"`

	// Orderer config
	NumBuckets     int           `yaml:"NumBuckets"`
	BatchSize      int           `yaml:"BatchSize"` // Maximum number of requests per batch
	BatchTimeoutMs int           `yaml:"BatchTimeout"`
	BatchTimeout   time.Duration // Timeout (ms) to cut batch when the bucket has less requests than the BatchSize.

	// PBFT Instance config
	DisabledViewChange  bool          `yaml:"DisabledViewChange"`
	ViewChangeTimeoutMs int           `yaml:"ViewChangeTimeout"`
	ViewChangeTimeout   time.Duration // Timeout (ns) to start a view change when an instance is not progressing.

	// Tracing
	EventBufferSize     int `yaml:"EventBufferSize"`     // Capacity of the tracing event buffer, in number of events.
	TraceSampling       int `yaml:"TraceSampling"`       // Only trace one out of TraceSampling events.
	ClientTraceSampling int `yaml:"ClientTraceSampling"` // Only trace one out of ClientTraceSampling events.

	// Client configuration
	ClientsPerProcess    int    `yaml:"ClientsPerProcess"`    // Number of concurrent clients in the orderingclient process (running as separate threads).
	RequestsPerClient    int    `yaml:"RequestsPerClient"`    // Number of requests each client submits.
	ClientRunTime        int    `yaml:"ClientRunTime"`        // Timeout for client to submit all its requests, in milliseconds. Set to 0 for no timeout.
	ClientDrainTime     int    `yaml:"ClientDrainTime"`     // Time (ms) to keep connections open after stopping submissions, to drain responses.
	RequestRate          int    `yaml:"RequestRate"`          // Max request rate per client, in requests per second.
	HardRequestRateLimit bool   `yaml:"HardRequestRateLimit"` // Never exceed rate limit or temporarily increase rate to catch up.
	RequestPayloadSize   int    `yaml:"RequestPayloadSize"`   // Size of the (randomly generated) request payload in bytes.
	SignRequests         bool   `yaml:"SignRequests"`
	VerifyRequestsEarly  bool   `yaml:"VerifyRequestsEarly"` // Verify request signatures before adding them to the bucket.
	ClientPubKeyFile     string `yaml:"ClientPubKeyFile"`    // Key for client request verification.
	ClientPrivKeyFile    string `yaml:"ClientPrivKeyFile"`   // Key for client request verification.
	PrecomputeRequests   bool   `yaml:"PrecomputeRequests"`  // Pre-compute (and sign, if applicable) all requests at a client before starting to submit.
	CrossOpRatio         int    `yaml:"CrossOpRatio"`        // Percentage (0-100) of client requests that are cross-group (TX K1,K2) instead of single-group (GET K).
	CrossOpGroupWeights  string `yaml:"CrossOpGroupWeights"` // Weights "groups:weight,groups:weight,..." for how many distinct groups a cross-op touches (e.g. "2:70,3:25,4:5"). Empty/unset defaults to "2:100" (always 2 groups, current behavior).

	// System parameters
	RequestHandlerThreads     int    `yaml:"RequestHandlerThreads"` // Number of threads that write incoming requests to request Buffers.
	RequestInputChannelBuffer int    `yaml:"RequestInputChannelBuffer"`
	BatchVerifier             string `yaml:"BatchVerifier"`

	// Proxy configuration for cross-op GSN assignment
	CrossOpProxyNodeID int32 `yaml:"CrossOpProxyNodeID"` // Node ID that acts as proxy for cross-op GSN assignment. Set to -1 to disable (all nodes assign GSN).

	// Maximum size (bytes) of a cross-op batch proposed by CutBatchWithMode. 0 (or unset) falls back to 32KB.
	CrossOpBatchMaxBytes int `yaml:"CrossOpBatchMaxBytes"`

	// Sequencer leader election (GSN sequencer failover)
	SequencerHeartbeatMs       int           `yaml:"SequencerHeartbeatMs"`       // Leader heartbeat period. 0 (or unset) falls back to 100ms.
	SequencerHeartbeat         time.Duration // Derived from SequencerHeartbeatMs.
	SequencerElectionTimeoutMs int           `yaml:"SequencerElectionTimeoutMs"` // Min follower election timeout (randomized [min,2*min]). 0 (or unset) falls back to 500ms.
	SequencerElectionTimeout   time.Duration // Derived from SequencerElectionTimeoutMs.
}

func LoadFile(configFileName string) {
	f, err := ioutil.ReadFile(configFileName)
	if err != nil {
		logger.Fatal().Err(err).Str("configFileName", configFileName).Msg("Could not read config file.")
	}

	err = yaml.Unmarshal(f, &Config)
	if err != nil {
		logger.Fatal().Err(err).Str("configFileName", configFileName).Msg("Could not unmarshal config file.")
	}

	logger.Debug().Str("Logging", Config.LoggingLevelStr).Msg("Config")
	logger.Debug().Bool("UseTLS", Config.UseTLS).Msg("Config")
	logger.Debug().Str("CACertFile", Config.CACertFile).Msg("Config")
	logger.Debug().Str("KeyFile", Config.KeyFile).Msg("Config")
	logger.Debug().Str("CertFile", Config.CertFile).Msg("Config")
	logger.Debug().Int("PriorityConnections", Config.PriorityConnections).Msg("Config")
	logger.Debug().Int("BasicConnections", Config.BasicConnections).Msg("Config")
	logger.Debug().Bool("TestConnections", Config.TestConnections).Msg("Config")
	logger.Debug().Int("ExcessConnections", Config.ExcessConnections).Msg("Config")
	logger.Debug().Int("ConnectionTestMsgs", Config.ConnectionTestMsgs).Msg("Config")
	logger.Debug().Int("ConnectionTestPayload", Config.ConnectionTestPayload).Msg("Config")
	logger.Debug().Int("OutMessageBufsize", Config.OutMessageBufSize).Msg("Config")
	logger.Debug().Int("OutMessageBatchPeriod", Config.OutMessageBatchPeriod).Msg("Config")
	logger.Debug().Int("ThroughputCap", Config.ThroughputCap).Msg("Config")
	logger.Debug().Int("StragglerTolerance", Config.StragglerTolerance).Msg("Config")
	logger.Debug().Int("BatchSizeIncrement", Config.BatchSizeIncrement).Msg("Config")
	logger.Debug().Str("Orderer", Config.Orderer).Msg("Config")
	logger.Debug().Str("Manager", Config.Manager).Msg("Config")
	logger.Debug().Int("Failures", Config.Failures).Msg("Config")
	logger.Debug().Str("CrashTiming", Config.CrashTiming).Msg("Config")
	logger.Debug().Int("CheckpointInterval", Config.CheckpointInterval).Msg("Config")
	logger.Debug().Int("WatermarkWindowSize", Config.WatermarkWindowSize).Msg("Config")
	logger.Debug().Int("EpochLength", Config.EpochLength).Msg("Config")
	logger.Debug().Int("SegmentLength", Config.SegmentLength).Msg("Config")
	logger.Debug().Bool("WaitForCheckpoints", Config.WaitForCheckpoints).Msg("Config")
	logger.Debug().Int("ClientWatermarkWindowSize", Config.ClientWatermarkWindowSize).Msg("Config")
	logger.Debug().Int("ClientRequestBacklogSize", Config.ClientRequestBacklogSize).Msg("Config")
	logger.Debug().Int64("RandomSeed", Config.RandomSeed).Msg("Config")
	logger.Debug().Int("NodeToLeaderRatio", Config.NodeToLeaderRatio).Msg("Config")
	logger.Debug().Str("LeaderPolicy", Config.LeaderPolicy).Msg("Config")
	logger.Debug().Int("DefaultLeaderBan", Config.DefaultLeaderBan).Msg("Config")
	logger.Debug().Int("NumBuckets", Config.NumBuckets).Msg("Config")
	logger.Debug().Int("BatchTimeout", Config.BatchTimeoutMs).Msg("Config")
	logger.Debug().Bool("DisabledViewChange", Config.DisabledViewChange).Msg("Config")
	logger.Debug().Int("ViewChangeTimeout", Config.ViewChangeTimeoutMs).Msg("Config")
	logger.Debug().Int("ClientTraceSampling", Config.ClientTraceSampling).Msg("Config")
	logger.Debug().Int("EventBufferSize", Config.EventBufferSize).Msg("Config")
	logger.Debug().Int("TraceSampling", Config.TraceSampling).Msg("Config")
	logger.Debug().Int("ClientsPerProcess", Config.ClientsPerProcess).Msg("Config")
	logger.Debug().Int("RequestsPerClient", Config.RequestsPerClient).Msg("Config")
	logger.Debug().Int("ClientRunTime", Config.ClientRunTime).Msg("Config")
	logger.Debug().Int("ClientDrainTime", Config.ClientDrainTime).Msg("Config")
	logger.Debug().Int("RequestRate", Config.RequestRate).Msg("Config")
	logger.Debug().Bool("HardRequestRateLimit", Config.HardRequestRateLimit).Msg("Config")
	logger.Debug().Int("RequestPayloadSize", Config.RequestPayloadSize).Msg("Config")
	logger.Debug().Bool("SignRequests", Config.SignRequests).Msg("Config")
	logger.Debug().Str("ClientPrivKeyFile", Config.ClientPrivKeyFile).Msg("Config")
	logger.Debug().Str("ClientPubKeyFile", Config.ClientPubKeyFile).Msg("Config")
	logger.Debug().Bool("PrecomputeRequests", Config.PrecomputeRequests).Msg("Config")
	logger.Debug().Int("RequestHandlerThreads", Config.RequestHandlerThreads).Msg("Config")
	logger.Debug().Int("RequestInputChannelBuffer", Config.RequestInputChannelBuffer).Msg("Config")
	logger.Debug().Str("BatchVerifier", Config.BatchVerifier).Msg("Config")
	logger.Debug().Int32("CrossOpProxyNodeID", Config.CrossOpProxyNodeID).Msg("Config")
	logger.Debug().Int("CrossOpBatchMaxBytes", Config.CrossOpBatchMaxBytes).Msg("Config")
	logger.Debug().Int("SequencerHeartbeatMs", Config.SequencerHeartbeatMs).Msg("Config")
	logger.Debug().Int("SequencerElectionTimeoutMs", Config.SequencerElectionTimeoutMs).Msg("Config")
	logger.Debug().Int("CrossOpRatio", Config.CrossOpRatio).Msg("Config")
	logger.Debug().Str("CrossOpGroupWeights", Config.CrossOpGroupWeights).Msg("Config")

	Config.LoggingLevel = setLoggingLevel(Config.LoggingLevelStr)

	Config.BatchTimeout = time.Duration(Config.BatchTimeoutMs) * time.Millisecond
	Config.ViewChangeTimeout = time.Duration(Config.ViewChangeTimeoutMs) * time.Millisecond

	if Config.CrossOpBatchMaxBytes <= 0 {
		Config.CrossOpBatchMaxBytes = 32 * 1024
	}

	if Config.SequencerHeartbeatMs <= 0 {
		Config.SequencerHeartbeatMs = 100
	}
	Config.SequencerHeartbeat = time.Duration(Config.SequencerHeartbeatMs) * time.Millisecond

	if Config.SequencerElectionTimeoutMs <= 0 {
		Config.SequencerElectionTimeoutMs = 500
	}
	Config.SequencerElectionTimeout = time.Duration(Config.SequencerElectionTimeoutMs) * time.Millisecond

	parseCrossOpGroupWeights(Config.CrossOpGroupWeights)
}

func setLoggingLevel(level string) zerolog.Level {
	switch level {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		logger.Fatal().Msg("Unsupported logging level")
	}
	return zerolog.NoLevel
}

// crossOpGroupWeight is one "N groups : weight" entry of CrossOpGroupWeights, with
// threshold holding the cumulative weight up to and including this entry.
type crossOpGroupWeight struct {
	groups    int
	threshold uint32
}

var crossOpGroupCumulative []crossOpGroupWeight
var crossOpGroupTotalWeight uint32

// parseCrossOpGroupWeights parses a spec like "2:70,3:25,4:5" into cumulative weight
// ranges used by PickCrossOpGroupCount. An empty spec (or one with no valid entries)
// falls back to "2:100", matching the pre-existing behavior of every cross-op always
// touching exactly 2 groups. Entries with fewer than 2 groups or non-positive weight
// are ignored, since touching fewer than 2 groups would not be a cross-op at all.
func parseCrossOpGroupWeights(spec string) {
	var cumulative uint32
	var parsed []crossOpGroupWeight
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		groups, errG := strconv.Atoi(strings.TrimSpace(kv[0]))
		weight, errW := strconv.Atoi(strings.TrimSpace(kv[1]))
		if errG != nil || errW != nil || groups < 2 || weight <= 0 {
			continue
		}
		cumulative += uint32(weight)
		parsed = append(parsed, crossOpGroupWeight{groups: groups, threshold: cumulative})
	}
	if len(parsed) == 0 {
		parsed = []crossOpGroupWeight{{groups: 2, threshold: 1}}
		cumulative = 1
	}
	crossOpGroupCumulative = parsed
	crossOpGroupTotalWeight = cumulative
}

// PickCrossOpGroupCount deterministically picks how many distinct groups the cross-op
// with the given client sequence number should touch, according to CrossOpGroupWeights.
// Deterministic (hash of seqNr, not real randomness) so that, like CrossOpRatio itself,
// the same configuration always reproduces the same distribution across repeated
// experiments, and the choice here doesn't correlate with the CrossOpRatio check
// (which uses seqNr directly, not a hash of it).
func PickCrossOpGroupCount(seqNr int32) int {
	h := crc32.ChecksumIEEE([]byte(fmt.Sprintf("crossopgroups-%d", seqNr)))
	pick := h % crossOpGroupTotalWeight
	for _, w := range crossOpGroupCumulative {
		if pick < w.threshold {
			return w.groups
		}
	}
	return crossOpGroupCumulative[len(crossOpGroupCumulative)-1].groups
}
