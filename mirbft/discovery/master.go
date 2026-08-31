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

package discovery

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

// Bounds the blocking send in enqueueMasterCommand. If a slave's NextCommand goroutine is not
// currently waiting to receive (e.g. its gRPC stream dropped and hasn't reconnected/re-entered the
// receive yet), the send would otherwise block forever, freezing the single sequential dispatcher
// goroutine for every slave and every subsequent command -- not just the stuck one. This was found
// to be a real, previously-unpatched hang distinct from the responseWG/peerWg/syncWg races already
// fixed (none of those timeouts cover this code path).
const enqueueCommandTimeout = 30 * time.Second

// waitWithTimeout waits for wg with a deadline.
// Returns true if wg reached zero, false if timeout expired.
func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

func ParseCommandStr(cmdStr string, tokenChannel chan string) {

	// Trim all white space from the start and end of cmdStr.
	cmdStr = strings.TrimSpace(cmdStr)

	// Skip empty lines and comments.
	// A string starting with '#', optionally preceded by white space, is considered to be a comment
	if len(cmdStr) == 0 || cmdStr[0] == '#' {
		return
	}

	// Handle "exec-start" and "exec-wait" as special cases.
	// Only split it in 3 tokens:
	// 1) command name ("exec-start" or "exec-wait")
	// 2) slave tag
	// 3) output file for redirecting stdout and stderr or timeout
	// 4) all the rest of the string  (the actual command to execute or more master commands to
	//                                 execute if a timeout occurs - probably contains spaces)
	if strings.HasPrefix(cmdStr, "exec-start") || strings.HasPrefix(cmdStr, "exec-wait") {
		fields := strings.Fields(cmdStr)
		tokenChannel <- fields[0] // "exec-start"             "exec-wait"
		tokenChannel <- fields[1] // slave tag          or    slave tag
		tokenChannel <- fields[2] // output file name         timeout value
		commandData := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(cmdStr, fields[0])), fields[1])), fields[2])
		tokenChannel <- commandData // the command to execute

		// For all other inputs, just split them into tokens and feed the tokens into the token channel for processing.
	} else {
		for _, t := range strings.Fields(cmdStr) {
			tokenChannel <- t
		}
	}
}

// Starts a user command processing goroutine.
// Returns a channel to which user commands can be written in form of string tokens: command name followed parameters.
// Processing finishes when the channel is closed.
// Calls Done() on the provided WaitGroup when tha last command has been processed.
// ATTENTION: Must not be called a second time before the returned channel is closed and empty!
//            That might result in undefined behavior, as both instances would be operating on the same slave state.
func (ds *DiscoveryServer) ProcessCommands(wg *sync.WaitGroup) chan string {
	tokens := make(chan string)

	go func() {
		for cmd, ok := <-tokens; ok; cmd, ok = <-tokens {
			ds.processCommand(cmd, tokens)
		}
		wg.Done()
	}()

	return tokens
}

// Processes a user command.
// cmdName is the name of the command.
// Parameters (their number depends on the command) are read from the params channel.
func (ds *DiscoveryServer) processCommand(cmdName string, params chan string) {

	logger.Debug().Str("cmdName", cmdName).Msg("Entering function.")

	// Master command that will potentially be sent to slaves
	// Every master command (even if not sent) gets a unique ID that will be referenced in slaves' status message.
	mc := &pb.MasterCommand{CmdId: <-ds.cmdIDs}

	// Master command will be only enqueued at the end of this function if the code below sets the Cmd field.
	mc.Cmd = nil

	// Only peers with this tag will receive the command.
	// Usually obtained through the params channel.
	tag := ""

	// The "exec-wait" command is able to process supplementary commands if a timeout occurs.
	// In such a case the command string is stored here and executed at the tail of this function.
	timeoutCommands := ""

	// How long the trailing "Wait for response if necessary" block (below) waits for responseWG.
	// Defaults to 120s (covers sync/noop and any command with no meaningful timeout of its own).
	// The "exec-wait" case overrides this with the command's own timeout plus margin: that timeout
	// is what the SLAVE uses locally to decide when to escalate SIGINT/SIGKILL to its child process
	// (e.g. CLIENT_TIMEOUT=180000ms in generate-master-commands.py), so if this master-side wait is
	// shorter, the master gives up and moves on to the NEXT script command (e.g. the next
	// experiment's mkdir/config-save) while the slave is still legitimately busy running the
	// PREVIOUS one -- which is exactly what caused the flood of "Timeout pushing command" warnings
	// observed for the client slave once its own exec-wait ran past 120s under saturation.
	responseTimeout := 120 * time.Second

	// Based on the first token, decide which command to send to the slaves.
	switch cmdName {

	// Stop and shut down the slave process.
	case "stop":
		mc.Cmd = &pb.MasterCommand_Stop{Stop: &pb.Stop{}}
		tag = <-params
		logger.Info().
			Str("cmdName", "stop").
			Str("tag", tag).
			Msg("Processing command.")

	// Launch a program on the slave's machine (but do not wait until it finishes).
	// The program is represented as a single command string, e.g. "echo 'Foobar'"
	// The program is started by the slave process from within the go process using exec,
	// so no fancy shell constructs like pipes or output redirection work.
	// ATTENTION: The arguments also don't support spaces (in which case they are split in two separate arguments).
	// The format of this command is exec-start <output_file_name> <command_to_execute>.
	// The output of <command_to_execute> (both stderr and stdout)
	// will be stored in <output_file_name> on the slave machine.
	case "exec-start":
		tag = <-params
		outFileName := <-params
		cmdString := <-params
		fields := strings.Fields(cmdString)
		mc.Cmd = &pb.MasterCommand_ExecStart{ExecStart: &pb.ExecStart{
			Name:           fields[0],
			OutputFileName: outFileName,
			Args:           fields[1:],
		}}
		logger.Info().
			Str("cmdName", "exec-start").
			Str("tag", tag).
			Str("cmd", cmdString).
			Msg("Processing command.")

	// Wait until the last executed program (started using exec-start) finishes.
	case "exec-wait":
		tag = <-params
		timeoutStr := <-params
		timeoutCommands = strings.TrimSpace(<-params)

		var timeout int
		var err error

		if timeout, err = strconv.Atoi(timeoutStr); err != nil {
			logger.Error().Str("timeout", timeoutStr).Msg("Cannot parse timeout (must be a number).")
		}

		mc.Cmd = &pb.MasterCommand_ExecWait{ExecWait: &pb.ExecWait{Timeout: int32(timeout)}}
		logger.Info().
			Str("cmdName", "exec-wait").
			Str("tag", tag).
			Str("timeoutCommands", timeoutCommands).
			Msg("Processing command.")

		if ds.lastSignalSent == "SIGKILL" {
			logger.Info().Msg("Skipping exec-wait block after SIGKILL.")
		} else {
			// Wait until this command returns and potentially react by executing timeoutCommands
			atomic.StoreInt32(&ds.waitingForCmd, mc.CmdId)
			atomic.StoreInt32(&ds.maxCommandExitStatus, 0)
			ds.responseWG = &sync.WaitGroup{}
			ds.responseWG.Add(ds.countSlaves(tag))

			// Give the master-side wait at least as long as the slave's own local timeout for
			// this command, plus margin for network/dispatch overhead. See responseTimeout comment
			// above for why a shorter fixed wait here causes the master to move on prematurely.
			if cmdTimeout := time.Duration(timeout)*time.Millisecond + 30*time.Second; cmdTimeout > responseTimeout {
				responseTimeout = cmdTimeout
			}
		}

	// Send a signal to the process executing the program (started using exec-start)
	// See pb.ExecSignal_Signum_value for possible parameter values.
	case "exec-signal":
		tag = <-params
		sigVal := <-params
		mc.Cmd = &pb.MasterCommand_ExecSignal{
			ExecSignal: &pb.ExecSignal{Signum: pb.ExecSignal_Signum(pb.ExecSignal_Signum_value[sigVal])},
		}
		ds.lastSignalSent = sigVal
		logger.Info().
			Str("cmdName", "exec-signal").
			Str("tag", tag).
			Str("sigVal", sigVal).
			Msg("Processing command.")

	// Wait for a certain number of slaves to connect or for a time period
	// Format :
	// - wait for slaves tag 100    (100 can be replaced by any number of slaves to wait for)
	// - wait for 30s           (30s can be replaced by anything parsable by time.ParseDuration, like 2h45m, 300ms, ...)
	case "wait":
		<-params // Consume the word "for" (just for aesthetic reasons)
		param := <-params
		switch param {
		case "slaves":
			slaveTag := <-params
			numSlavesStr := <-params

			logger.Info().
				Str("cmdName", "wait for slaves").
				Str("tag", slaveTag).
				Str("n", numSlavesStr).
				Msg("Processing command.")

			if n, err := strconv.Atoi(numSlavesStr); err == nil {
				ds.waitForSlaves(slaveTag, n)
				logger.Info().Str("tag", slaveTag).Int("numSlaves", n).Msg("Finished waiting for slaves.")
			} else {
				logger.Error().Str("numSlaves", numSlavesStr).Msg("Cannot parse number of slaves.")
			}
		default:
			logger.Info().
				Str("cmdName", "wait for some time").
				Str("t", param).
				Msg("Processing command.")

			d, err := time.ParseDuration(param)
			if err == nil {
				time.Sleep(d)
				logger.Info().Str("t", param).Msg("Finished waiting for time duration.")
			} else {
				logger.Error().Str("duration", param).Msg("Cannot parse time duration.")
			}
		}

	// Takes one argument - the number of peers to discover next - and resets all relevant state
	// (including peer and client ID counters).
	// After this command, the server is ready to be used as for discovery.
	case "discover-reset":
		numPeersStr := <-params

		logger.Info().
			Str("cmdName", "discover-reset").
			Str("nPeers", numPeersStr).
			Msg("Processing command.")

		if n, err := strconv.Atoi(numPeersStr); err == nil {
			ds.resetPC(n)
		} else {
			logger.Error().Str("numSlaves", numPeersStr).Msg("Cannot parse number of slaves.")
		}

	// Waits until all peers (their number must have been specified by a previous discover-reset command) connect.
	case "discover-wait":
		logger.Info().
			Str("cmdName", "discover-wait").
			Msg("Processing command.")

		if !waitWithTimeout(&ds.peerWg, 120*time.Second) {
			logger.Warn().Msg("Timeout waiting for peers to register (discover-wait peerWg); continuing.")
		}
		logger.Info().Msg("All peer processes started. Waiting until they connect to each other (discover-wait).")
		if !waitWithTimeout(&ds.syncWg, 120*time.Second) {
			logger.Warn().Msg("Timeout waiting for peers to sync (discover-wait syncWg); continuing.")
		}
		logger.Info().Msg("Peers connected to each other. Done waiting (discover-wait).")

	// Sends a NOOP command to the slaves and waits until they respond.
	case "sync":
		tag = <-params

		logger.Info().
			Str("cmdName", "sync").
			Str("tag", tag).
			Msg("Processing command.")

		mc.Cmd = &pb.MasterCommand_Noop{Noop: &pb.Noop{}}
		atomic.StoreInt32(&ds.waitingForCmd, mc.CmdId)
		ds.responseWG = &sync.WaitGroup{}
		ds.responseWG.Add(ds.countSlaves(tag))

	case "write-file":
		filename := <-params
		content := <-params

		logger.Info().
			Str("cmdName", "write-file").
			Str("fileName", filename).
			Str("content", content).
			Msg("Processing command.")

		if f, err := os.Create(filename); err != nil {
			logger.Error().Err(err).Str("filename", filename).Msg("Could not open file for writing.")
		} else {
			if _, err := f.WriteString(content); err != nil {
				logger.Error().
					Err(err).
					Str("filename", filename).
					Str("content", content).
					Msg("Could not write content to file.")
			}
			f.Close()
		}

	// Unknown command
	default:
		logger.Error().Str("cmd", cmdName).Msg("Unknown command.")
	}

	// If the command results in sending a MasterCommand, enqueue the obtained MasterCommand for sending to all slaves.
	if mc.Cmd != nil {

		// Enqueue command.
		ds.enqueueMasterCommand(tag, mc)

		// Wait for response if necessary.
		if ds.responseWG != nil {
			logger.Debug().Msg("Waiting for response.")
			rwg := ds.responseWG
			if !waitWithTimeout(rwg, responseTimeout) {
				logger.Warn().Str("cmd", mc.String()).Dur("timeout", responseTimeout).Msg("Timeout waiting for slave responses; continuing.")
			}
			logger.Debug().Msg("response received.")
			ds.mu.Lock()
			atomic.StoreInt32(&ds.waitingForCmd, -1)
			ds.responseWG = nil
			ds.mu.Unlock()
		}
	}

	logger.Debug().Str("timeoutCommands", fmt.Sprintf("'%s'", timeoutCommands)).Msg("Processed command.")

	// If the command set the timeoutCommands variable and a timeout occurred,
	// execute the supplementary timeout command
	if timeoutCommands != "" && atomic.LoadInt32(&ds.maxCommandExitStatus) > 0 {
		wg := sync.WaitGroup{}
		wg.Add(1)
		newTempCommandProcessor := ds.ProcessCommands(&wg)
		for _, c := range strings.Split(timeoutCommands, ";") {
			ParseCommandStr(c, newTempCommandProcessor)
		}
		close(newTempCommandProcessor)
		wg.Wait()
	}
}

// Enqueues a command for sending to each slave with a specific
// tag when that slave asks for it (by invoking NextCommand).
// The command is also sent to the slaves already waiting for the next command.
func (ds *DiscoveryServer) enqueueMasterCommand(tag string, mc *pb.MasterCommand) {

	// Enqueue the command to every peer with the correct tag
	i := 0
	j := 0
	ds.slaves.Range(func(key interface{}, value interface{}) bool {
		i++
		if tag == WildcardAllTags || value.(*slave).Tag == tag {
			j++
			sl := value.(*slave)
			logger.Debug().
				Int32("slaveID", sl.SlaveID).
				Str("cmd", mc.String()).
				Str("tag", tag).
				Msg("Pushing command to slave.")

			start := time.Now()
			select {
			case sl.CommandQueue <- mc:
				if elapsed := time.Since(start); elapsed > time.Second {
					logger.Warn().
						Int32("slaveID", sl.SlaveID).
						Str("tag", tag).
						Dur("waited", elapsed).
						Msg("Slave took unusually long to pick up command (but did).")
				}
			case <-time.After(enqueueCommandTimeout):
				// This slave's NextCommand goroutine is not currently waiting to receive --
				// most likely its gRPC stream dropped and hasn't reconnected past this point yet.
				// Skip it instead of freezing the dispatcher (and every other slave) forever.
				logger.Warn().
					Int32("slaveID", sl.SlaveID).
					Str("tag", tag).
					Str("cmd", mc.String()).
					Dur("timeout", enqueueCommandTimeout).
					Msg("Timeout pushing command to slave (not currently receiving); skipping this slave for this command.")
			}
		}
		return true
	})

	logger.Debug().Str("cmd", mc.String()).Int("numSlaves", i).Int("numPushes", j).Msg("Finished pushing command to slaves.")
}

// Waits until n slaves are connected.
// Every second, counts the slaves and returns if at least n has been reached.
func (ds *DiscoveryServer) waitForSlaves(tag string, n int) {
	for {
		i := 0
		// The function used in Range increments i for each slave whose tag matches the tag given in the parameter.
		ds.slaves.Range(func(k interface{}, v interface{}) bool {
			if tag == WildcardAllTags || v.(*slave).Tag == tag {
				i++
			}
			return true
		})
		if i >= n {
			return
		}
		time.Sleep(time.Second)
	}
}

// Returns the number of slaves with the given tag.
func (ds *DiscoveryServer) countSlaves(tag string) int {
	counter := 0

	ds.slaves.Range(func(key interface{}, value interface{}) bool {
		if tag == WildcardAllTags || value.(*slave).Tag == tag {
			counter++
		}
		return true
	})

	return counter
}

