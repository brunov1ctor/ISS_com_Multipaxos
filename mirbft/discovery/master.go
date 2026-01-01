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
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

func ParseCommandStr(cmdStr string, tokenChannel chan string) {

	// Trim all white space from the start and end of cmdStr.
	cmdStr = strings.TrimSpace(cmdStr)

	// Skip empty lines and comments.
	// A string starting with '#', optionally preceded by white space, is considered to be a comment
	if len(cmdStr) == 0 || cmdStr[0] == '#' {
		return
	}

	// Handle "exec-start" and "exec-wait" as special cases
	// These commands read a string from standard input and store it in the token channel.
	// The string read can then be referenced in later commands (as a token).
	// This is useful for synchronizing commands with external events.
	// For example, the command sequence:
	//
	// exec-start
	// exec-wait
	// exec-finish
	//
	// waits for an external event between exec-start and exec-wait.
	// Specifically:
	//
	// - exec-start reads a token from standard input and stores it in the token channel.
	// - exec-wait reads a token from standard input and blocks until it matches the token in the token channel.
	// - exec-finish reads a token from standard input and stores it in the token channel.
	//
	// This can be used to synchronize the execution of commands with an external process.
	// For example, if the external process writes tokens to standard input at specific times,
	// the master can wait for those tokens before proceeding.

	if cmdStr == "exec-start" || cmdStr == "exec-wait" || cmdStr == "exec-finish" {
		fmt.Println("exec:" + cmdStr)
		// Read a token from stdin.
		reader := bufio.NewReader(os.Stdin)
		token, err := reader.ReadString('\n')
		if err != nil {
			panic(err)
		}
		token = strings.TrimSpace(token)

		if cmdStr == "exec-wait" {
			// Wait until the token matches the one in the token channel.
			for {
				t := <-tokenChannel
				if t == token {
					break
				}
			}
		} else {
			// Store token in the token channel.
			tokenChannel <- token
		}
		return
	}

	tokens := strings.Fields(cmdStr)
	if len(tokens) == 0 {
		return
	}

	// First token is the command type.
	cmdType := tokens[0]

	switch cmdType {

	case "sleep":
		// sleep <duration>
		// Sleeps for the given duration. Duration is parsed using time.ParseDuration.
		if len(tokens) != 2 {
			panic(fmt.Sprintf("invalid sleep command: %s", cmdStr))
		}
		dur, err := time.ParseDuration(tokens[1])
		if err != nil {
			panic(err)
		}
		time.Sleep(dur)

	case "write-file":
		// write-file <path> <content...>
		// Writes content to a file at path. Creates directories as needed.
		if len(tokens) < 3 {
			panic(fmt.Sprintf("invalid write-file command: %s", cmdStr))
		}
		path := tokens[1]
		content := strings.Join(tokens[2:], " ")
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err != nil {
			panic(err)
		}
		err = os.WriteFile(path, []byte(content), 0o644)
		if err != nil {
			panic(err)
		}

	default:
		panic(fmt.Sprintf("unknown command type: %s", cmdType))
	}
}

func ParseCommandsFromFile(filename string) ([]*pb.MasterCommand, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var cmds []*pb.MasterCommand

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)

		// Skip empty lines and comments.
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		cmd, err := ParseMasterCommand(line)
		if err != nil {
			return nil, err
		}
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return cmds, nil
}

func ParseMasterCommand(cmdStr string) (*pb.MasterCommand, error) {

	// Trim all white space from the start and end of cmdStr.
	cmdStr = strings.TrimSpace(cmdStr)

	// Skip empty lines and comments.
	if len(cmdStr) == 0 || cmdStr[0] == '#' {
		return nil, nil
	}

	tokens := strings.Fields(cmdStr)
	if len(tokens) == 0 {
		return nil, nil
	}

	cmdType := tokens[0]

	switch cmdType {

	case "start-slave":
		// start-slave <exec> <args...>
		if len(tokens) < 2 {
			return nil, errors.New("invalid start-slave command")
		}
		exec := tokens[1]
		args := []string{}
		if len(tokens) > 2 {
			args = tokens[2:]
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_START_SLAVE,
			Arg:  exec,
			Args: args,
		}, nil

	case "exec-slave":
		// exec-slave <id> <exec> <args...>
		if len(tokens) < 3 {
			return nil, errors.New("invalid exec-slave command")
		}
		id, err := strconv.ParseUint(tokens[1], 10, 32)
		if err != nil {
			return nil, err
		}
		exec := tokens[2]
		args := []string{}
		if len(tokens) > 3 {
			args = tokens[3:]
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_EXEC_SLAVE,
			Id:   uint32(id),
			Arg:  exec,
			Args: args,
		}, nil

	case "kill-slave":
		// kill-slave <id>
		if len(tokens) != 2 {
			return nil, errors.New("invalid kill-slave command")
		}
		id, err := strconv.ParseUint(tokens[1], 10, 32)
		if err != nil {
			return nil, err
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_KILL_SLAVE,
			Id:   uint32(id),
		}, nil

	case "remove-slave":
		// remove-slave <id>
		if len(tokens) != 2 {
			return nil, errors.New("invalid remove-slave command")
		}
		id, err := strconv.ParseUint(tokens[1], 10, 32)
		if err != nil {
			return nil, err
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_REMOVE_SLAVE,
			Id:   uint32(id),
		}, nil

	case "broadcast":
		// broadcast <exec> <args...>
		if len(tokens) < 2 {
			return nil, errors.New("invalid broadcast command")
		}
		exec := tokens[1]
		args := []string{}
		if len(tokens) > 2 {
			args = tokens[2:]
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_BROADCAST,
			Arg:  exec,
			Args: args,
		}, nil

	case "broadcast-wait":
		// broadcast-wait <exec> <args...>
		if len(tokens) < 2 {
			return nil, errors.New("invalid broadcast-wait command")
		}
		exec := tokens[1]
		args := []string{}
		if len(tokens) > 2 {
			args = tokens[2:]
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_BROADCAST_WAIT,
			Arg:  exec,
			Args: args,
		}, nil

	case "wait":
		// wait <id>
		if len(tokens) != 2 {
			return nil, errors.New("invalid wait command")
		}
		id, err := strconv.ParseUint(tokens[1], 10, 32)
		if err != nil {
			return nil, err
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_WAIT,
			Id:   uint32(id),
		}, nil

	case "wait-all":
		// wait-all
		if len(tokens) != 1 {
			return nil, errors.New("invalid wait-all command")
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_WAIT_ALL,
		}, nil

	case "sleep":
		// sleep <duration>
		if len(tokens) != 2 {
			return nil, errors.New("invalid sleep command")
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_SLEEP,
			Arg:  tokens[1],
		}, nil

	case "write-file":
		// write-file <path> <content...>
		if len(tokens) < 3 {
			return nil, errors.New("invalid write-file command")
		}
		path := tokens[1]
		content := strings.Join(tokens[2:], " ")
		return &pb.MasterCommand{
			Type: pb.MasterCommand_WRITE_FILE,
			Arg:  path,
			Args: []string{content},
		}, nil

	case "delete-file":
		// delete-file <path>
		if len(tokens) != 2 {
			return nil, errors.New("invalid delete-file command")
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_DELETE_FILE,
			Arg:  tokens[1],
		}, nil

	case "copy-file":
		// copy-file <src> <dst>
		if len(tokens) != 3 {
			return nil, errors.New("invalid copy-file command")
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_COPY_FILE,
			Arg:  tokens[1],
			Args: []string{tokens[2]},
		}, nil

	case "copy-dir":
		// copy-dir <src> <dst>
		if len(tokens) != 3 {
			return nil, errors.New("invalid copy-dir command")
		}
		return &pb.MasterCommand{
			Type: pb.MasterCommand_COPY_DIR,
			Arg:  tokens[1],
			Args: []string{tokens[2]},
		}, nil

	case "exec-start":
		return &pb.MasterCommand{
			Type: pb.MasterCommand_EXEC_START,
		}, nil

	case "exec-wait":
		return &pb.MasterCommand{
			Type: pb.MasterCommand_EXEC_WAIT,
		}, nil

	case "exec-finish":
		return &pb.MasterCommand{
			Type: pb.MasterCommand_EXEC_FINISH,
		}, nil

	default:
		return nil, fmt.Errorf("unknown command: %s", cmdType)
	}
}

