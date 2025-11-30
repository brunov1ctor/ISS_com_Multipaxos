package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/discovery"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"google.golang.org/grpc"
)

const (
	numIDDigits = 3
)

// ensureExperimentOutputDirs ensures that all parent directories for any
// argument containing "experiment-output/" exist on disk before starting
// the commanded process. This prevents failures like:
//   open experiment-output/0000/slave-001/peer.log: no such file or directory
// and makes it much easier to reason about why orderingpeer/orderingclient
// did not produce traces.
func ensureExperimentOutputDirs(args []string) {
	for _, a := range args {
		if strings.Contains(a, "experiment-output/") {
			dir := filepath.Dir(a)
			if err := os.MkdirAll(dir, 0755); err != nil {
				logger.Error().
					Err(err).
					Str("dir", dir).
					Msg("failed to create experiment-output directory")
			} else {
				logger.Debug().
					Str("dir", dir).
					Msg("ensured experiment-output directory exists")
			}
		}
	}
}

func main() {
	// Configure logger
	zerolog.TimeFieldFormat = time.RFC3339
	logger.Logger = logger.Logger.With().Str("component", "discoveryslave").Logger()

	if len(os.Args) != 5 {
		fmt.Fprintf(os.Stderr, "Usage: %s <tag> <masterAddr> <publicIP> <privateIP>\n", os.Args[0])
		os.Exit(1)
	}

	slvTag := os.Args[1]
	masterAddr := os.Args[2]
	publicIP := os.Args[3]
	privateIP := os.Args[4]

	if !strings.Contains(masterAddr, ":") {
		masterAddr = fmt.Sprintf("%s:%d", masterAddr, discovery.DefaultDiscoveryPort)
	}

	logger.Info().
		Str("tag", slvTag).
		Str("masterAddr", masterAddr).
		Str("publicIP", publicIP).
		Str("privateIP", privateIP).
		Msg("starting discoveryslave")

	// Connect to gRPC discovery master
	conn, err := grpc.Dial(masterAddr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to connect to discovery master")
	}
	defer conn.Close()

	client := pb.NewDiscoveryClient(conn)

	// Wildcards used for replacement in commands
	wildcards := map[string]string{
		discovery.WildcardSlavePublicIP:  publicIP,
		discovery.WildcardSlavePrivateIP: privateIP,
	}

	var (
		slvID        int64 = -1
		lastStatus   int32 = 0
		lastMsg      string
		lastCmdID    int64
		execCmd      *exec.Cmd
		execOutFile  *os.File
		execStartSet bool
	)

	ctx := context.Background()

	// Initial registration: SlaveId = -1
	logger.Debug().Msg("sending initial SlaveStatus to master")
	resp, err := client.NextCommand(ctx, &pb.SlaveStatus{
		SlaveId: -1,
		Status:  0,
		Message: "",
		Tag:     slvTag,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to get initial command from master")
	}

	logger.Info().
		Int64("cmdId", resp.Id).
		Stringer("cmdType", resp.GetCommandType()).
		Msg("received initial command from master")

	if initCmd := resp.GetInitSlave(); initCmd != nil {
		slvID = initCmd.SlaveId

		wildcards[discovery.WildcardSlaveID] = fmt.Sprintf("%0*d", numIDDigits, slvID)

		logger.Info().
			Int64("slaveId", slvID).
			Msg("initialized slave")
	} else {
		logger.Fatal().Msg("first command from master was not InitSlave")
	}

	lastCmdID = resp.Id

	// Main command loop
	for {
		var statusMsg string

		if lastMsg != "" {
			statusMsg = lastMsg
		}

		logger.Debug().
			Int64("slaveId", slvID).
			Int32("status", lastStatus).
			Int64("cmdId", lastCmdID).
			Str("msg", statusMsg).
			Msg("sending SlaveStatus to master")

		resp, err = client.NextCommand(ctx, &pb.SlaveStatus{
			SlaveId: slvID,
			Status:  lastStatus,
			Message: statusMsg,
			Tag:     slvTag,
			CmdId:   lastCmdID,
		})
		if err != nil {
			logger.Fatal().Err(err).Msg("NextCommand failed")
		}

		logger.Info().
			Int64("cmdId", resp.Id).
			Stringer("cmdType", resp.GetCommandType()).
			Msg("received command from master")

		lastCmdID = resp.Id
		lastStatus = 0
		lastMsg = ""

		switch cmd := resp.Command.(type) {

		case *pb.MasterCommand_InitSlave:
			// Should normally only happen once, but we handle it anyway
			slvID = cmd.InitSlave.SlaveId
			wildcards[discovery.WildcardSlaveID] = fmt.Sprintf("%0*d", numIDDigits, slvID)
			logger.Info().
				Int64("slaveId", slvID).
				Msg("InitSlave command in loop")

		case *pb.MasterCommand_ExecStart:
			if execCmd != nil && execStartSet {
				logger.Warn().Msg("ExecStart received but previous command still running; killing it")
				_ = execCmd.Process.Kill()
			}

			execStartSet = false

			// Replace wildcards in args and output file name
			cmd.ExecStart.OutputFileName = replaceWildcards(cmd.ExecStart.OutputFileName, wildcards)
			for i, arg := range cmd.ExecStart.Args {
				cmd.ExecStart.Args[i] = replaceWildcards(arg, wildcards)
			}

			// Ensure experiment-output directories exist before starting the process.
			// This is especially important for orderingpeer / orderingclient.
			ensureExperimentOutputDirs(cmd.ExecStart.Args)

			logger.Debug().
				Str("execName", cmd.ExecStart.Name).
				Str("execOutFile", cmd.ExecStart.OutputFileName).
				Str("execArgs", fmt.Sprint(cmd.ExecStart.Args)).
				Msg("prepared ExecStart command (directories ensured)")

			// Create Command to execute
			execCmd = exec.Command(cmd.ExecStart.Name, cmd.ExecStart.Args...)

			// Attach output file if specified
			if cmd.ExecStart.OutputFileName != "" {
				f, err := os.Create(cmd.ExecStart.OutputFileName)
				if err != nil {
					logger.Error().
						Err(err).
						Str("outfile", cmd.ExecStart.OutputFileName).
						Msg("failed to create output file for ExecStart")

					lastStatus = 1
					lastMsg = fmt.Sprintf("failed to create output file: %v", err)
					execCmd = nil
					execOutFile = nil
					break
				}
				execOutFile = f
				execCmd.Stdout = f
				execCmd.Stderr = f
			} else {
				execOutFile = nil
			}

			// Start process
			if err := execCmd.Start(); err != nil {
				logger.Error().
					Err(err).
					Str("name", cmd.ExecStart.Name).
					Str("args", fmt.Sprint(cmd.ExecStart.Args)).
					Msg("ExecStart failed to start process")

				lastStatus = 1
				lastMsg = fmt.Sprintf("failed to start command: %v", err)
				execCmd = nil
				if execOutFile != nil {
					_ = execOutFile.Close()
					execOutFile = nil
				}
				break
			}

			execStartSet = true
			logger.Info().
				Int("pid", execCmd.Process.Pid).
				Str("name", cmd.ExecStart.Name).
				Msg("ExecStart started process")

		case *pb.MasterCommand_ExecWait:
			if execCmd == nil || !execStartSet {
				logger.Warn().Msg("ExecWait without active process")
				lastStatus = 1
				lastMsg = "ExecWait received but no process is running"
				break
			}

			err := execCmd.Wait()
			execStartSet = false

			if execOutFile != nil {
				_ = execOutFile.Close()
				execOutFile = nil
			}

			if err != nil {
				// Non-zero exit
				if exitErr, ok := err.(*exec.ExitError); ok {
					ws := exitErr.Sys().(syscall.WaitStatus)
					code := ws.ExitStatus()
					lastStatus = int32(code)
					lastMsg = fmt.Sprintf("command exited with status %d", code)
					logger.Warn().
						Int("status", code).
						Msg("ExecWait: process exited with non-zero status")
				} else {
					lastStatus = 1
					lastMsg = fmt.Sprintf("ExecWait error: %v", err)
					logger.Error().Err(err).Msg("ExecWait: error waiting for process")
				}
			} else {
				lastStatus = 0
				lastMsg = "command finished successfully"
				logger.Info().Msg("ExecWait: process finished successfully")
			}

		case *pb.MasterCommand_ExecSignal:
			if execCmd == nil || !execStartSet {
				logger.Warn().Msg("ExecSignal received but no process is running")
				lastStatus = 1
				lastMsg = "ExecSignal but no running process"
				break
			}

			sig := syscall.Signal(cmd.ExecSignal.Signal)
			if err := execCmd.Process.Signal(sig); err != nil {
				logger.Error().
					Err(err).
					Int32("signal", cmd.ExecSignal.Signal).
					Msg("failed to send signal to process")
				lastStatus = 1
				lastMsg = fmt.Sprintf("failed to signal process: %v", err)
			} else {
				logger.Info().
					Int32("signal", cmd.ExecSignal.Signal).
					Msg("signal sent to process")
				lastStatus = 0
				lastMsg = "signal sent"
			}

		case *pb.MasterCommand_Sleep:
			d := time.Duration(cmd.Sleep.Milliseconds) * time.Millisecond
			logger.Debug().
				Int64("sleepMs", cmd.Sleep.Milliseconds).
				Msg("Sleep command received")
			time.Sleep(d)
			lastStatus = 0
			lastMsg = fmt.Sprintf("slept for %dms", cmd.Sleep.Milliseconds)

		case *pb.MasterCommand_Noop:
			logger.Debug().Msg("Noop command")
			lastStatus = 0
			lastMsg = "noop"

		case *pb.MasterCommand_Stop:
			logger.Info().Msg("Stop command received, terminating discoveryslave")
			lastStatus = 0
			lastMsg = "stopping"
			return

		default:
			logger.Error().
				Stringer("cmdType", resp.GetCommandType()).
				Msg("unknown command type from master")
			lastStatus = 1
			lastMsg = "unknown command type"
		}
	}
}

// replaceWildcards replaces discovery wildcards like __SLAVE_ID__,
// __SLAVE_PUBLIC_IP__, __SLAVE_PRIVATE_IP__ etc. in a string.
func replaceWildcards(s string, wildcards map[string]string) string {
	for k, v := range wildcards {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

