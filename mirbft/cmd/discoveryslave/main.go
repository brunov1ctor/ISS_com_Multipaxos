package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"path/filepath"

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
		logger.Fatal().Err(err).Msg("Could not create gRPC connection to master.")
	}
	defer func() {
		if err := conn.Close(); err != nil {
			logger.Error().Err(err).Msg("Error closing gRPC connection.")
		}
	}()

	// Instantiate a discovery client.
	discoveryClient := pb.NewDiscoveryServiceClient(conn)

	// Register this slave instance with the master.
	logger.Info().Msg("Submitting slave registration request.")

	regResp, err := discoveryClient.RegisterSlave(context.Background(),
		&pb.SlaveRegistration{
			SlaveTag:     slaveTag,
			OwnPublicIp:  ownPublicIP,
			OwnPrivateIp: ownPrivateIP,
		})
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to register slave at master.")
	}

	// Confirm validity of response.
	if regResp.OwnId < 0 {
		logger.Fatal().Int32("slaveID", regResp.OwnId).Msg("Slave got invalid ID from the master.")
	}

	// Create map of wildcard replacements local to this slave.
	wildcards := make(map[string]string)
	wildcards[discovery.WildcardSlaveID] = slaveIDString(regResp.OwnId)
	wildcards[discovery.WildcardPublicIP] = ownPublicIP
	wildcards[discovery.WildcardPrivateIP] = ownPrivateIP

	logger.Info().
		Int32("ownID", regResp.OwnId).
		Str("publicIP", ownPublicIP).
		Str("privateIP", ownPrivateIP).
		Msg("Slave successfully registered at master.")

	// Enter command execution loop: Ask master server for next command, execute it, ask for the next command, etc...
	var cmdID int32
	var execCmd *exec.Cmd
	var execOutFile *os.File
	var cmdOutFile *os.File
	var exitStatus int32
	var exitMessage string

	for {
		log := logger.With().
			Str("tag", slaveTag).
			Int32("ownID", regResp.OwnId).
			Int32("cmdId", cmdID).
			Logger()

		// Ask master for next command.
		log.Info().Msg("Asking master for next command.")
		nextCmd, err := discoveryClient.NextCommand(context.Background(),
			&pb.CommandRequest{
				CmdId: cmdID,
			})
		if err != nil {
			logger.Fatal().Err(err).Msg("Failed to get next command from the master.")
		} else {
			logger.Info().Int32("cmdId", nextCmd.CmdId).Msg("Received command.")
		}

		// Save command id for submitting it in the next command request.
		cmdID = nextCmd.CmdId

		// Execute command, depending on type.
		switch cmd := nextCmd.Cmd.(type) {

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
				// E.g., discovery.WildcardSlaveID (__id__ at the time of writing this comment)
				// is replaced by own slave ID.
				cmd.ExecStart.OutputFileName = replaceWildcards(cmd.ExecStart.OutputFileName, wildcards)

				for i, arg := range cmd.ExecStart.Args {
					cmd.ExecStart.Args[i] = replaceWildcards(arg, wildcards)
				}

				logger.Info().
					Str("resolvedOutFile", cmd.ExecStart.OutputFileName).
					Str("resolvedArgs", fmt.Sprint(cmd.ExecStart.Args)).
					Msg("ExecStart after wildcard replacement")

				// Create Command to execute
				execCmd = exec.Command(cmd.ExecStart.Name, cmd.ExecStart.Args...)

				// Ensure the output directory for ExecStart exists (robust against missing experiment-output/*/slave-__id__).
				outDir := filepath.Dir(cmd.ExecStart.OutputFileName)
				if err = os.MkdirAll(outDir, 0755); err != nil {
					logger.Error().
						Err(err).
						Str("outDir", outDir).
						Str("outFileName", cmd.ExecStart.OutputFileName).
						Msg("Could not create directory for ExecStart output file")
				}
				logger.Info().
					Str("outDir", outDir).
					Str("outFileName", cmd.ExecStart.OutputFileName).
					Msg("ExecStart output directory ensured")

				// Open output file (will be closed when executing the ExecWait command)
				if execOutFile, err = os.Create(cmd.ExecStart.OutputFileName); err != nil {
					logger.Error().
						Err(err).
						Str("outFileName", cmd.ExecStart.OutputFileName).
						Msg("Could not open file for writing")
					// Redirect Command's output to file
				} else {
					execCmd.Stdout = execOutFile
					execCmd.Stderr = execOutFile
				}

				// Launch Command
				if err = execCmd.Start(); err != nil {
					exitMessage = "Failed to start command: " + err.Error()
					exitStatus = 2
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
				exitMessage = "No command running."
				exitStatus = 1
			} else {

				status := execCmd.ProcessState

				if !status.Exited() {
					// Wait for started program to terminate.

					var waitStatus syscall.WaitStatus

					if _, err := execCmd.Process.Wait(); err != nil {
						logger.Error().Err(err).Msg("Error waiting for process.")
						exitMessage = "Error waiting for process."
						exitStatus = 2
					} else {
						if st := execCmd.ProcessState.Sys(); st != nil {
							waitStatus, _ = st.(syscall.WaitStatus)
							exitStatus = int32(waitStatus.ExitStatus())
							exitMessage = "Program exited normally."
						} else {
							exitMessage = "Program status unknown."
							exitStatus = 3
						}
					}
				} else {
					exitMessage = "Program not running."
					exitStatus = int32(status.ExitCode())
				}

				if exitStatus == 0 && err != nil {
					exitMessage = "Error waiting for program: " + err.Error()
					exitStatus = 4
				} else if exitStatus == 0 {
					if err := execOutFile.Close(); err != nil {
						logger.Error().Err(err).Msg("Could not close output file.")
						exitMessage = "Could not close output file."
						exitStatus = 5
					} else {
						exitMessage = "OK"
					}
				}
			}

		// Send signal to program running in the background.
		case *pb.MasterCommand_Signal:
			exitStatus = 0
			logger.Info().Str("cmdType", "Signal").Msg("Received command.")
			if execCmd == nil {
				exitMessage = "No command running."
				exitStatus = 1
			} else {
				// Send signal to program.
				sig, err := parseSignal(cmd.Signal.SignalName)
				if err != nil {
					exitMessage = "Unknown signal: " + cmd.Signal.SignalName
					exitStatus = 1
				}

				if err := execCmd.Process.Signal(sig); err != nil {
					exitMessage = "Error sending signal to program: " + err.Error()
					exitStatus = 2
				} else {
					exitMessage = "OK"
				}
			}

		// Transfer a local file to the master.
		case *pb.MasterCommand_Sendfile:
			exitStatus = 0
			logger.Info().Str("cmdType", "Sendfile").Msg("Received command.")

			// Replace wildcards in file name.
			cmd.Sendfile.SrcName = replaceWildcards(cmd.Sendfile.SrcName, wildcards)

			// Attempt to open file for reading.
			if cmdOutFile, err = os.Open(cmd.Sendfile.SrcName); err != nil {
				logger.Error().Err(err).Str("outFileName", cmd.Sendfile.SrcName).Msg("Could not open file for reading")
				exitMessage = "Could not open file for reading: " + err.Error()
				exitStatus = 1
			} else {

				// Create log entry including file name and target ID.
				targetIDs := make([]string, len(cmd.Sendfile.TargetIds))
				for i, id := range cmd.Sendfile.TargetIds {
					targetIDs[i] = strconv.FormatInt(int64(id), 10)
				}
				logger.Info().
					Str("srcFile", cmd.Sendfile.SrcName).
					Str("targetIDs", strings.Join(targetIDs, ",")).
					Msg("Sending file to master server.")

				// Perform the file transfer.
				_, err := discoveryClient.StreamSlaveFile(context.Background(),
					&pb.FileStream{
						SrcId:     regResp.OwnId,
						TargetIds: cmd.Sendfile.TargetIds,
						Name:      cmd.Sendfile.StringName,
						Contents:  cmdOutFile,
					})
				if err != nil {
					logger.Error().Err(err).Msg("Could not send file to master.")
					exitMessage = "Could not send file: " + err.Error()
					exitStatus = 2
				} else {
					exitMessage = "OK"
				}
			}

		default:
			logger.Error().
				Str("cmdType", fmt.Sprintf("%T", cmd)).
				Msg("Received unknown command type.")
			exitStatus = 1
			exitMessage = fmt.Sprint("Unknown command:", cmd)
		}
	}
}

func replaceWildcards(data string, mapping map[string]string) string {
	for orig, repl := range mapping {
		// Could have used ReplaceAll, but reverted to this for compatibility with old Go versions.
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

// parseSignal is helper to translate signal names (e.g. "SIGTERM") to syscall.Signal.
func parseSignal(sigName string) (syscall.Signal, error) {
	switch sigName {
	case "SIGTERM":
		return syscall.SIGTERM, nil
	case "SIGINT":
		return syscall.SIGINT, nil
	case "SIGKILL":
		return syscall.SIGKILL, nil
	case "SIGQUIT":
		return syscall.SIGQUIT, nil
	default:
		return 0, fmt.Errorf("unknown signal %s", sigName)
	}
}

