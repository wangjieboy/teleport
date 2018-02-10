/*
Copyright 2017 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package service

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/sirupsen/logrus"
)

// printShutdownStatus prints running services until shut down
func (process *TeleportProcess) printShutdownStatus(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Infof("Waiting for services: %v to finish.", process.Supervisor.Services())
		}
	}
}

// WaitForSignals waits for system signals and processes them
func (process *TeleportProcess) WaitForSignals(ctx context.Context) error {
	sigC := make(chan os.Signal, 1024)
	signal.Notify(sigC, os.Interrupt, os.Kill, syscall.SIGTERM, syscall.SIGUSR2, syscall.SIGCHLD, syscall.SIGHUP)

	doneContext, cancel := context.WithCancel(ctx)
	defer cancel()

	// Block until a signal is received or handler got an error
	for {
		select {
		case signal := <-sigC:
			switch signal {
			case syscall.SIGTERM, syscall.SIGINT:
				go process.printShutdownStatus(doneContext)
				process.Shutdown(ctx)
				log.Infof("All services stopped, exiting.")
				return nil
			case syscall.SIGKILL, syscall.SIGQUIT:
				log.Infof("Got signal %q, exiting immediately.", signal)
				process.Close()
				return nil
			case syscall.SIGUSR2:
				log.Infof("Got signal %q, forking a new process.", signal)
				if err := process.forkChild(); err != nil {
					log.Infof("Failed to fork: %s", trace.DebugReport(err))
				} else {
					log.Infof("Successfully started new process.")
				}
			case syscall.SIGHUP:
				log.Infof("Got signal %q, performing graceful restart.", signal)
				if err := process.forkChild(); err != nil {
					log.Infof("Failed to fork: %s", trace.DebugReport(err))
				} else {
					log.Infof("Successfully started new process.")
				}
				log.Infof("Shutting down gracefully.")
				go process.printShutdownStatus(doneContext)
				process.Shutdown(ctx)
				log.Infof("All services stopped, exiting.")
				return nil
			case syscall.SIGCHLD:
				log.Debugf("Child exited, got %q, collecting status.", signal)
				var wait syscall.WaitStatus
				syscall.Wait4(-1, &wait, syscall.WNOHANG, nil)
			default:
				log.Infof("Ignoring %q.", signal)
			}
		case <-ctx.Done():
			process.Close()
			process.Wait()
			log.Info("Got request to shutdown, context is closing")
			return nil
		}
	}
}

// importOrCreateListener imports listener passed by the parent process (happens during live reload)
// or creates a new listener if there was no listener registered
func (process *TeleportProcess) importOrCreateListener(listenerType, address string) (net.Listener, error) {
	l, err := process.importListener(listenerType, address)
	if err == nil {
		log.Infof("Using file descriptor %v %v passed by the parent process.", listenerType, address)
		return l, nil
	}
	if !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}
	return process.createListener(listenerType, address)
}

// importListener imports listener passed by the parent process, if no listener is found
// returns NotFound, otherwise removes the file from the list
func (process *TeleportProcess) importListener(listenerType, address string) (net.Listener, error) {
	process.Lock()
	defer process.Unlock()

	for i := range process.importedDescriptors {
		d := process.importedDescriptors[i]
		if d.Type == listenerType && d.Address == address {
			l, err := d.ToListener()
			if err != nil {
				return nil, trace.Wrap(err)
			}
			process.importedDescriptors = append(process.importedDescriptors[:i], process.importedDescriptors[i+1:]...)
			process.registeredListeners = append(process.registeredListeners, RegisteredListener{Type: listenerType, Address: address, Listener: l})
			return l, nil
		}
	}

	return nil, trace.NotFound("no file descriptor for type %v and address %v has been imported", listenerType, address)
}

// createListener creates listener and adds to a list of tracked listeners
func (process *TeleportProcess) createListener(listenerType, address string) (net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	process.Lock()
	defer process.Unlock()
	r := RegisteredListener{Type: listenerType, Address: address, Listener: listener}
	process.registeredListeners = append(process.registeredListeners, r)
	return listener, nil
}

// exportFileDescriptors exports file descriptors to be passed to child process
func (process *TeleportProcess) exportFileDescriptors() ([]FileDescriptor, error) {
	var out []FileDescriptor
	process.Lock()
	defer process.Unlock()
	for _, r := range process.registeredListeners {
		file, err := utils.GetListenerFile(r.Listener)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		out = append(out, FileDescriptor{File: file, Type: r.Type, Address: r.Address})
	}
	return out, nil
}

// importFileDescriptors imports file descriptors from environment if there are any
func importFileDescriptors() ([]FileDescriptor, error) {
	// These files may be passed in by the parent process
	filesString := os.Getenv(teleportFilesEnvVar)
	if filesString == "" {
		return nil, nil
	}

	files, err := filesFromString(filesString)
	if err != nil {
		return nil, trace.BadParameter("child process has failed to read files, error %q", err)
	}

	if len(files) != 0 {
		log.Infof("Child has been passed files: %v", files)
	}

	return files, nil
}

type RegisteredListener struct {
	Type     string
	Address  string
	Listener net.Listener
}

type FileDescriptor struct {
	Type    string
	Address string
	File    *os.File
}

func (fd *FileDescriptor) ToListener() (net.Listener, error) {
	listener, err := net.FileListener(fd.File)
	if err != nil {
		return nil, err
	}
	fd.File.Close()
	return listener, nil
}

type fileDescriptor struct {
	Address  string `json:"addr"`
	Type     string `json:"type"`
	FileFD   int    `json:"fd"`
	FileName string `json:"fileName"`
}

// filesToString serializes file descriptors as well as accompanying information (like socket host and port)
func filesToString(files []FileDescriptor) (string, error) {
	out := make([]fileDescriptor, len(files))
	for i, f := range files {
		out[i] = fileDescriptor{
			// Once files will be passed to the child process and their FDs will change.
			// The first three passed files are stdin, stdout and stderr, every next file will have the index + 3
			// That's why we rearrange the FDs for child processes to get the correct file descriptors.
			FileFD:   i + 3,
			FileName: f.File.Name(),
			Address:  f.Address,
			Type:     f.Type,
		}
	}
	bytes, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

const teleportFilesEnvVar = "TELEPORT_OS_FILES"

func execPath() (string, error) {
	name, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	if _, err = os.Stat(name); nil != err {
		return "", err
	}
	return name, err
}

// filesFromString de-serializes the file descriptors and turns them in the os.Files
func filesFromString(in string) ([]FileDescriptor, error) {
	var out []fileDescriptor
	if err := json.Unmarshal([]byte(in), &out); err != nil {
		return nil, err
	}
	files := make([]FileDescriptor, len(out))
	for i, o := range out {
		files[i] = FileDescriptor{
			File:    os.NewFile(uintptr(o.FileFD), o.FileName),
			Address: o.Address,
			Type:    o.Type,
		}
	}
	return files, nil
}

func (process *TeleportProcess) forkChild() error {
	path, err := execPath()
	if err != nil {
		return trace.Wrap(err)
	}

	workingDir, err := os.Getwd()
	if nil != err {
		return err
	}

	log := log.WithFields(logrus.Fields{"path": path, "workingDir": workingDir})

	log.Infof("Forking child.")

	listenerFiles, err := process.exportFileDescriptors()
	if err != nil {
		return trace.Wrap(err)
	}

	// These files will be passed to the child process
	files := []*os.File{os.Stdin, os.Stdout, os.Stderr}
	for _, f := range listenerFiles {
		files = append(files, f.File)
	}

	// Serialize files to JSON string representation
	vals, err := filesToString(listenerFiles)
	if err != nil {
		return err
	}

	log.Infof("Passing %s to child", vals)
	os.Setenv(teleportFilesEnvVar, vals)

	p, err := os.StartProcess(path, os.Args, &os.ProcAttr{
		Dir:   workingDir,
		Env:   os.Environ(),
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})

	if err != nil {
		return trace.ConvertSystemError(err)
	}

	log.WithFields(logrus.Fields{"pid": p.Pid}).Infof("Started new child process.")
	return nil
}
