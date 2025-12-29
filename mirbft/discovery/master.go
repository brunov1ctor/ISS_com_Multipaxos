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
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

func ParseCommandStr(cmdStr string, tokenChannel chan string) {

	// Trim all white space from the start and end of cmdStr.
	cmdStr = strings.TrimSpace(cmdStr)

	// Skip empty lines and comments.
	// A string starting with '#', optionally preceded by white space, is considered to be a comment
	if len(cmdStr) == 0 || cmdStr[0] == '#' {
		return
	}

	// Split cmdStr into tokens (shell-like, supports quoting).
	tokens, err := splitCommandLine(cmdStr)
	if err != nil {
		logger.Error().Err(err).Msgf("Could not parse command line: %s", cmdStr)
		return
	}

	for _, token := range tokens {
		tokenChannel <- token
	}
}

type SlaveStatus struct {
	ID        uint64
	AddrPort  string
	PublicIP  string
	PrivateIP string
	Status    int64
	LastCmdID uint64
	Tag       string
}

type DiscoveryServer struct {
	pb.UnimplementedDiscoveryServer

	Listener    net.Listener
	CommandChan chan *pb.Command

	slavesLock      sync.Mutex
	slaves          map[string]*SlaveStatus
	slaveDisconnect chan string
}

func NewDiscoveryServer() *DiscoveryServer {
	return &DiscoveryServer{
		CommandChan:      make(chan *pb.Command, 128),
		slaves:           make(map[string]*SlaveStatus),
		slaveDisconnect:  make(chan string, 128),
		slavesLock:       sync.Mutex{},
		UnimplementedDiscoveryServer: pb.UnimplementedDiscoveryServer{},
	}
}

func (ds *DiscoveryServer) Start(ctx context.Context, port uint64) error {

	var err error
	ds.Listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	logger.Info().Msgf("Starting server. port=%d", port)

	go func() {
		for {
			select {
			case <-ctx.Done():
				_ = ds.Listener.Close()
				return

			case addrPort := <-ds.slaveDisconnect:
				ds.slavesLock.Lock()
				delete(ds.slaves, addrPort)
				ds.slavesLock.Unlock()
			}
		}
	}()

	return nil
}

func (ds *DiscoveryServer) processCommand(cmdString string) error {

	cmdString = strings.TrimSpace(cmdString)
	if len(cmdString) == 0 || cmdString[0] == '#' {
		return nil
	}

	cmdParts, err := splitCommandLine(cmdString)
	if err != nil {
		return fmt.Errorf("%s: could not parse: %w", cmdString, err)
	}
	if len(cmdParts) == 0 {
		return nil
	}

	switch {

	case cmdParts[0] == "write-file":
		if len(cmdParts) < 3 {
			return fmt.Errorf("%s: too few tokens", cmdString)
		}
		filename := cmdParts[1]
		content := strings.Join(cmdParts[2:], " ")

		command := &pb.Command{
			Command: &pb.Command_WriteFile{
				WriteFile: &pb.WriteFile{
					FileName: filename,
					Content:  content,
				},
			},
		}

		ds.CommandChan <- command

	case cmdParts[0] == "wait-for-slaves":
		if len(cmdParts) < 4 {
			return fmt.Errorf("%s: too few tokens", cmdString)
		}
		tag := cmdParts[1]
		n := cmdParts[2]
		timeout := cmdParts[3]

		nSlaves := uint64(0)
		if _, err := fmt.Sscanf(n, "%d", &nSlaves); err != nil {
			return fmt.Errorf("%s: cannot parse n as uint64", cmdString)
		}

		timeoutDuration := time.Duration(0)
		if _, err := fmt.Sscanf(timeout, "%d", &timeoutDuration); err != nil {
			return fmt.Errorf("%s: cannot parse timeout as int64", cmdString)
		}
		timeoutDuration = timeoutDuration * time.Millisecond

		command := &pb.Command{
			Command: &pb.Command_WaitForSlaves{
				WaitForSlaves: &pb.WaitForSlaves{
					N:       nSlaves,
					Timeout: timeoutDuration.Milliseconds(),
				},
			},
			Tag: tag,
		}

		ds.CommandChan <- command

	case cmdParts[0] == "exec-start":
		// Strict format:
		//   exec-start <tag> <outFile> <cmd> <args...>
		// outFile may be "-" to discard output.
		if len(cmdParts) < 4 {
			return fmt.Errorf("%s: invalid exec-start format; expected: exec-start <tag> <outFile> <cmd> <args...>", cmdString)
		}

		tag := cmdParts[1]
		outFile := cmdParts[2]
		fields := cmdParts[3:]

		command := &pb.Command{
			Command: &pb.Command_ExecStart{
				ExecStart: &pb.ExecStart{
					Name:    fields[0],
					Args:    fields[1:],
					OutFile: outFile,
				},
			},
			Tag: tag,
		}

		ds.CommandChan <- command

	case cmdParts[0] == "exec-wait":
		if len(cmdParts) < 3 {
			return fmt.Errorf("%s: too few tokens", cmdString)
		}
		tag := cmdParts[1]
		timeout := cmdParts[2]

		timeoutDuration := time.Duration(0)
		if _, err := fmt.Sscanf(timeout, "%d", &timeoutDuration); err != nil {
			return fmt.Errorf("%s: cannot parse timeout as int64", cmdString)
		}
		timeoutDuration = timeoutDuration * time.Millisecond

		command := &pb.Command{
			Command: &pb.Command_ExecWait{
				ExecWait: &pb.ExecWait{
					Timeout: timeoutDuration.Milliseconds(),
				},
			},
			Tag: tag,
		}

		ds.CommandChan <- command

	case cmdParts[0] == "sync":
		if len(cmdParts) < 2 {
			return fmt.Errorf("%s: too few tokens", cmdString)
		}
		tag := cmdParts[1]

		command := &pb.Command{
			Command: &pb.Command_Sync{
				Sync: &pb.Sync{},
			},
			Tag: tag,
		}

		ds.CommandChan <- command

	case cmdParts[0] == "discover-reset":
		if len(cmdParts) < 2 {
			return fmt.Errorf("%s: too few tokens", cmdString)
		}
		nPeers := uint64(0)
		if _, err := fmt.Sscanf(cmdParts[1], "%d", &nPeers); err != nil {
			return fmt.Errorf("%s: cannot parse nPeers as uint64", cmdString)
		}

		command := &pb.Command{
			Command: &pb.Command_DiscoverReset{
				DiscoverReset: &pb.DiscoverReset{
					NPeers: nPeers,
				},
			},
		}

		ds.CommandChan <- command

	case cmdParts[0] == "discover-wait":
		command := &pb.Command{
			Command: &pb.Command_DiscoverWait{
				DiscoverWait: &pb.DiscoverWait{},
			},
		}

		ds.CommandChan <- command

	default:
		return fmt.Errorf("%s: unrecognized command name", cmdString)
	}

	return nil
}

// splitCommandLine splits a command line into tokens similarly to a POSIX shell.
// It supports:
//   - whitespace separation
//   - single quotes: '...'
//   - double quotes: "..." (supports \\ and \" escapes)
//   - backslash escaping outside quotes (and inside double quotes)
// It does not perform variable expansion or globbing.
func splitCommandLine(s string) ([]string, error) {
	var out []string
	var cur strings.Builder

	inSingle := false
	inDouble := false
	escaping := false

	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}

	for i := 0; i < len(s); i++ {
		b := s[i]

		if escaping {
			cur.WriteByte(b)
			escaping = false
			continue
		}

		if b == '\\' {
			// Backslash escapes next char unless in single quotes.
			if inSingle {
				cur.WriteByte(b)
			} else {
				escaping = true
			}
			continue
		}

		if inSingle {
			if b == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(b)
			}
			continue
		}

		if inDouble {
			if b == '"' {
				inDouble = false
			} else {
				cur.WriteByte(b)
			}
			continue
		}

		// Outside quotes
		switch {
		case b == '\'':
			inSingle = true
		case b == '"':
			inDouble = true
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			flush()
		default:
			cur.WriteByte(b)
		}
	}

	if escaping {
		return nil, fmt.Errorf("dangling escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()

	// Also support the legacy token stream parser (ParseCommandStr) which expects
	// the original string to already be trimmed and comments removed.
	return out, nil
}

func (ds *DiscoveryServer) ProcessCommandFile(filename string) error {

	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	logger.Info().Msgf("Processing command file. file=%s", filename)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {

		cmdString := scanner.Text()

		if err := ds.processCommand(cmdString); err != nil {
			logger.Error().Err(err).Msgf("Error processing command. cmdString=%s", cmdString)
			return err
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func (ds *DiscoveryServer) RegisterSlave(ctx context.Context, status *pb.SlaveStatus) (*pb.RegisterSlaveResponse, error) {

	peer, ok := peerFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("could not retrieve peer from context")
	}

	addrPort := peer.AddrPort

	ds.slavesLock.Lock()
	defer ds.slavesLock.Unlock()

	ds.slaves[addrPort] = &SlaveStatus{
		ID:        status.ID,
		AddrPort:  addrPort,
		PublicIP:  status.PublicIP,
		PrivateIP: status.PrivateIP,
		Status:    status.Status,
		LastCmdID: status.LastCmdID,
		Tag:       status.Tag,
	}

	logger.Info().Msgf("New slave. addrPort=%s slaveID=%d tag=%s", addrPort, status.ID, status.Tag)

	return &pb.RegisterSlaveResponse{}, nil
}

func (ds *DiscoveryServer) GetNextCommand(ctx context.Context, req *pb.GetNextCommandRequest) (*pb.GetNextCommandResponse, error) {

	peer, ok := peerFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("could not retrieve peer from context")
	}

	addrPort := peer.AddrPort

	ds.slavesLock.Lock()
	slave, ok := ds.slaves[addrPort]
	if ok {
		slave.Status = req.Status
		slave.LastCmdID = req.LastCmdID
	}
	ds.slavesLock.Unlock()

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled")

	case cmd := <-ds.CommandChan:
		return &pb.GetNextCommandResponse{
			Command: cmd,
		}, nil
	}
}

func (ds *DiscoveryServer) WaitForSlaves(tag string, n uint64, timeout time.Duration) error {

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			return fmt.Errorf("timeout waiting for slaves")

		default:
			ds.slavesLock.Lock()
			count := uint64(0)
			for _, slave := range ds.slaves {
				if slave.Tag == tag {
					count++
				}
			}
			ds.slavesLock.Unlock()

			if count >= n {
				logger.Info().Msgf("Finished waiting for slaves. numSlaves=%d tag=%s", count, tag)
				return nil
			}

			time.Sleep(200 * time.Millisecond)
		}
	}
}

func (ds *DiscoveryServer) DisconnectSlave(addrPort string) {
	ds.slaveDisconnect <- addrPort
}

