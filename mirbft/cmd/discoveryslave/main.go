package main

import (
	"context"
	"fmt"
	"io"
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

func main() {

	// Configure logger
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	logger.Logger = logger.Output(zerolog.ConsoleWriter{Out: os.Stdout, NoColor: true})

	// Parse command line arguments.
	slaveTag := os.Args[1]
	masterAddr := os.Args[2]
	ownPublicIP := os.Args[3]
	ownPrivateIP := os.Args[4]

	// Set up a GRPC connection.
	conn, err := grpc.Dial(masterAddr, grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		logger.Fatal().Str("masterAddr", masterAddr).Msg("Could not connect to master server.")
	}
	defer conn.Close()

	// Register slave at the master by sending first SlaveStatus with ID = -1
	logger.Info().
		Str("slaveTag", slaveTag).
		Str("masterAddr", masterAddr).
		Str("publicIP", ownPublicIP).
		Str("privateIP", ownPrivateIP).
		Msg("Submitting initial slave status (registration).")

	discoveryClient := pb.NewDiscoveryClient(conn)

	// First call: register anonymous slave (SlaveId = -1)
	initStatus := &pb.SlaveStatus{
		CmdId:   -1,
		SlaveId: -1,
		Status:  0,
		Message: "initial registration",
		Tag:     slaveTag,
	}

	initResp, err := discoveryClient.NextCommand(context.Background(), initStatus)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to register slave at master (NextCommand).")
	}

	initCmd, ok := initResp.Cmd.(*pb.MasterCommand_InitSlave)
	if !ok {
		logger.Fatal().Msg("First command from master is not InitSlave; cannot determine slave ID.")
	}

	ownID := initCmd.InitSlave.SlaveId

	logger.Info().
		Int32("ownID", ownID).
		Str("publicIP", ownPublicIP).
		Str("privateIP", ownPrivateIP).
		Msg("Slave successfully registered at master.")

	// Create map of wildcard replacements local to this slave.
	wildcards := make(map[string]string)
	wildcards[discovery.WildcardSlaveID] = slaveIDString(ownID)
	wildcards[discovery.WildcardPublicIP] = ownPublicIP
	wildcards[discovery.WildcardPrivateIP] = ownPrivateIP

	// Enter command execution loop: ask master for next command, execute it, etc.
	var cmdID int32 = initResp.CmdId
	var execCmd *exec.Cmd
	var execOutFile *os.File
	var exitStatus int32
	var exitMessage string

cmdLoop:
	for {
		log := logger.With().
			Str("tag", slaveTag).
			Int32("ownID", ownID).
			Int32("lastCmdId", cmdID).
			Logger()

		// Ask master for next command, reporting the status of the previous one.
		log.Info().
			Int32("status", exitStatus).
			Str("message", exitMessage).
			Msg("Asking master for next command.")

		nextCmd, err := discoveryClient.NextCommand(context.Background(),
			&pb.SlaveStatus{
				CmdId:   cmdID,
				SlaveId: ownID,
				Status:  exitStatus,
				Message: exitMessage,
				Tag:     slaveTag,
			})
		if err != nil {
			logger.Error().
				Err(err).
				Msg("Failed to get next command from the master.")
			time.Sleep(time.Second)
			continue
		} else {
			logger.Info().Int32("cmdId", nextCmd.CmdId).Msg("Received command.")
		}

		// Save command id for submitting it in the next command request.
		cmdID = nextCmd.CmdId

		// Execute command, depending on type.
		switch cmd := nextCmd.Cmd.(type) {

		// Initialize slave - already handled on first call, but keep for completeness.
		case *pb.MasterCommand_InitSlave:
			logger.Info().
				Int32("newSlaveID", cmd.InitSlave.SlaveId).
				Msg("Received another InitSlave command.")
			ownID = cmd.InitSlave.SlaveId
			wildcards[discovery.WildcardSlaveID] = slaveIDString(ownID)
			exitMessage = "OK (InitSlave)"
			exitStatus = 0

		// Start a program in the background as a separate process.
		case *pb.MasterCommand_ExecStart:
			logger.Info().
				Str("cmd", cmd.ExecStart.Name).
				Str("outFile", cmd.ExecStart.OutputFileName).
				Str("args", fmt.Sprint(cmd.ExecStart.Args)).
				Msg("Received an ExecStart command")
			if execCmd != nil { // Some command already running
				exitMessage = "Ignoring command (another command already running)."
				exitStatus = 1
			} else { // No command running yet

				// Replace wildcards in output file name and arguments by local values.
				cmd.ExecStart.OutputFileName = replaceWildcards(cmd.ExecStart.OutputFileName, wildcards)
				for i, arg := range cmd.ExecStart.Args {
					cmd.ExecStart.Args[i] = replaceWildcards(arg, wildcards)
				}

				logger.Debug().
					Str("resolvedOutFile", cmd.ExecStart.OutputFileName).
					Str("resolvedArgs", fmt.Sprint(cmd.ExecStart.Args)).
					Msg("ExecStart after wildcard replacement")

				// Create Command to execute
				execCmd = exec.Command(cmd.ExecStart.Name, cmd.ExecStart.Args...)

				// Output handling:
				//  - If OutputFileName == "-" (or empty), discard output WITHOUT creating any file.
				//  - Otherwise, create the output file and redirect stdout/stderr there.
				if cmd.ExecStart.OutputFileName == "-" || cmd.ExecStart.OutputFileName == "" {
					execCmd.Stdout = io.Discard
					execCmd.Stderr = io.Discard
					execOutFile = nil
				} else {
					// Ensure output directory exists
					outDir := filepath.Dir(cmd.ExecStart.OutputFileName)
					if err := os.MkdirAll(outDir, 0755); err != nil {
						logger.Error().
							Err(err).
							Str("outDir", outDir).
							Msg("Could not create directory for ExecStart output file")
					} else {
						logger.Debug().
							Str("outDir", outDir).
							Str("outFileName", cmd.ExecStart.OutputFileName).
							Msg("ExecStart output directory ensured")
					}

					// Open output file (will be closed when executing the ExecWait command)
					if execOutFile, err = os.Create(cmd.ExecStart.OutputFileName); err != nil {
						logger.Error().
							Err(err).
							Str("outFileName", cmd.ExecStart.OutputFileName).
							Msg("Could not open file for writing")
						// Fallback: discard output (não aborta o comando só por falha de log)
						execCmd.Stdout = io.Discard
						execCmd.Stderr = io.Discard
						execOutFile = nil
					} else {
						execCmd.Stdout = execOutFile
						execCmd.Stderr = execOutFile
					}
				}

				// Launch Command
				if err = execCmd.Start(); err != nil {
					// ===== ALTERAÇÃO MÍNIMA AQUI =====
					// Se Start falhar, não pode deixar execCmd "armado", senão ExecWait vira "exec: not started".
					if execOutFile != nil {
						_ = execOutFile.Close()
						execOutFile = nil
					}
					execCmd = nil

					exitMessage = "Failed to start command: " + err.Error()
					exitStatus = 127
				} else {
					exitMessage = "OK"
					exitStatus = 0
				}
			}

		// Wait for program running in the background to finish.
		case *pb.MasterCommand_ExecWait:
			exitStatus = 0

			logger.Info().Str("cmdType", "ExecWait").Msg("Received command.")
			if execCmd == nil {
				// Processo já terminou antes do exec-wait chegar (ex: pkill rápido). OK.
				exitMessage = "OK (no program running)"
				exitStatus = 0
			} else {

				// Send a INT signal to the process after the timeout.
				timerInt := time.AfterFunc(time.Millisecond*time.Duration(cmd.ExecWait.Timeout), func() {
					_ = execCmd.Process.Signal(syscall.SIGINT)
				})

				timerKill := time.AfterFunc(time.Millisecond*2*time.Duration(cmd.ExecWait.Timeout), func() {
					_ = execCmd.Process.Signal(syscall.SIGKILL)
				})

				// Wait for started program to terminate.
				if err := execCmd.Wait(); err != nil {
					// Some commands intentionally use non-zero exit codes for benign outcomes.
					// Example: pkill returns 1 when no matching process exists.
					if exitErr, ok := err.(*exec.ExitError); ok {
						code := exitErr.ProcessState.ExitCode()
						cmdBase := filepath.Base(execCmd.Path)
						if isAcceptableExitCode(cmdBase, code) {
							logger.Info().Int("exitCode", code).Str("cmd", cmdBase).Msg("Process exited with acceptable non-zero exit code")
							exitMessage = fmt.Sprintf("OK (exit=%d)", code)
							exitStatus = 0
						} else {
							logger.Error().Err(err).Int("exitCode", code).Str("cmd", cmdBase).Msg("Error waiting for process")
							exitMessage = fmt.Sprintf("Error waiting for program (exit=%d): %s", code, err.Error())
							exitStatus = 2
						}
					} else {
						logger.Error().Err(err).Msg("Error waiting for process.")
						exitMessage = "Error waiting for program: " + err.Error()
						exitStatus = 2
					}
				} else {
					exitMessage = "Program exited normally."
					exitStatus = 0
				}

				timerInt.Stop()
				timerKill.Stop()

				if execOutFile != nil {
					if err := execOutFile.Close(); err != nil {
						logger.Error().Err(err).Msg("Could not close output file.")
						exitMessage = "Could not close output file."
						exitStatus = 3
					}
				}

				// Clear execCmd so that next ExecStart is accepted
				execCmd = nil
			}

		// Send signal to program running in the background.
		case *pb.MasterCommand_ExecSignal:
			exitStatus = 0
			logger.Info().Str("cmdType", "ExecSignal").Msg("Received command.")
			if execCmd == nil {
				// Processo já terminou antes do sinal chegar. OK.
				exitMessage = "OK (no program to signal)"
				exitStatus = 0
			} else {
				// Translate Signum enum to syscall.Signal
				var sig syscall.Signal
				switch cmd.ExecSignal.Signum {
				case pb.ExecSignal_SIGINT:
					sig = syscall.SIGINT
				case pb.ExecSignal_SIGKILL:
					sig = syscall.SIGKILL
				case pb.ExecSignal_SIGTERM:
					sig = syscall.SIGTERM
				default:
					exitMessage = "Unknown signal."
					exitStatus = 1
				}

				if exitStatus == 0 {
					if err := execCmd.Process.Signal(sig); err != nil {
						exitMessage = "Error sending signal to program: " + err.Error()
						exitStatus = 2
					} else {
						// Reap the child in background so it does not become a zombie.
						// ExecWait is not called after ExecSignal in the current protocol.
						cmdToReap := execCmd
						outFileToClose := execOutFile
						execCmd = nil
						execOutFile = nil
						go func() {
							_ = cmdToReap.Wait()
							if outFileToClose != nil {
								_ = outFileToClose.Close()
							}
						}()
						exitMessage = "OK"
					}
				}
			}

		// Do nothing (keep-alive / sync)
		case *pb.MasterCommand_Noop:
			logger.Info().Str("cmdType", "Noop").Msg("Received Noop command.")
			exitMessage = "OK (Noop)"
			exitStatus = 0

		// Stop slave loop.
		case *pb.MasterCommand_Stop:
			logger.Info().Str("cmdType", "Stop").Msg("Received Stop command.")
			exitMessage = "Stop"
			exitStatus = 0
			break cmdLoop

		default:
			logger.Warn().Msg("Received unknown command.")
			exitStatus = 1
			exitMessage = fmt.Sprint("Unknown command:", cmd)
		}
	}

	logger.Info().
		Int32("ownID", ownID).
		Msg("Slave loop terminated, exiting.")
}

// isAcceptableExitCode defines which non-zero exit codes are considered "OK"
// for certain executables. This prevents benign best-effort commands (like pkill)
// from poisoning the whole experiment run.
func isAcceptableExitCode(cmdBase string, code int) bool {
	// pkill returns 1 when no process matches the pattern.
	if cmdBase == "pkill" && code == 1 {
		return true
	}
	return false
}

func replaceWildcards(data string, mapping map[string]string) string {
	for orig, repl := range mapping {
		data = strings.Replace(data, orig, repl, -1)
	}
	return data
}

// slaveIDString returns a zero-padded string version of the slave ID
// (e.g. id=5 -> "005" se numIDDigits=3).
func slaveIDString(id int32) string {
	format := "%0" + strconv.Itoa(numIDDigits) + "d"
	return fmt.Sprintf(format, id)
}

