// Copyright 2016-2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux windows

// The default (ESX) implementation of the VmdkCmdRunner interface.
// This implementation sends synchronous commands to and receives responses from ESX.

package vmdkops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	log "github.com/sirupsen/logrus"
)

/*
#cgo CFLAGS: -I ../vmci
#cgo windows LDFLAGS: -L${SRCDIR} -lvmci_client
#include "vmci_client.h"
#include "vmci_client_proxy.c"
*/
import "C"

// Run command Guest VM requests on ESX via vmdkops_serv.py listening on vSocket
// *
// * For each request:
// *   - Establishes a vSocket connection
// *   - Sends json string up to ESX
// *   - waits for reply and returns resulting JSON or an error
func (vmdkCmd EsxVmdkCmd) Run(cmd string, name string, opts map[string]string) ([]byte, error) {
	vmdkCmd.Mtx.Lock()
	defer vmdkCmd.Mtx.Unlock()
	protocolVersion := os.Getenv("VDVS_TEST_PROTOCOL_VERSION")
	log.Debugf("Run get request: version=%s", protocolVersion)
	if protocolVersion == "" {
		protocolVersion = clientProtocolVersion
	}
	jsonStr, err := json.Marshal(&requestToVmci{
		Ops:     cmd,
		Details: VolumeInfo{Name: name, Options: opts},
		Version: protocolVersion})
	if err != nil {
		return nil, fmt.Errorf("Failed to marshal json: %v", err)
	}

	cmdS := C.CString(string(jsonStr))
	defer C.free(unsafe.Pointer(cmdS))

	beS := C.CString(commBackendName)
	defer C.free(unsafe.Pointer(beS))

	// Get the response data in json
	ans := (*C.be_answer)(C.calloc(1, C.sizeof_struct_be_answer))
	defer C.free(unsafe.Pointer(ans))

	var ret C.be_sock_status
	for i := 0; i <= maxRetryCount; i++ {
		ret, err = C.Vmci_GetReply(C.int(EsxPort), cmdS, beS, ans)
		if ret == 0 {
			// Received no error, exit loop.
			// C.Vmci_GetReply indicates success/faulure by <ret> value.
			// Cgo  interface adds <err> based on errno. We do not explicitly
			// reset errno in our code. Still, we do not want a stale errno
			// to confuse this code into thinking there was an error even when ret==0,
			// so explicitly declare success on <ret> value only, and
			break
		}

		var msg string
		if err != nil {
			var errno syscall.Errno
			errno = err.(syscall.Errno)
			msg = fmt.Sprintf("Run '%s' failed: %v (errno=%d) - %s", cmd, err, int(errno), C.GoString(&ans.errBuf[0]))
			if i < maxRetryCount {
				log.Warnf(msg + " Retrying...")
				time.Sleep(time.Second * 1)
				continue
			}
			if errno == syscall.ECONNRESET || errno == syscall.ETIMEDOUT {
				msg += " Cannot communicate with ESX, please refer to the FAQ https://github.com/vmware/docker-volume-vsphere/wiki#faq"
			}
		} else {
			msg = fmt.Sprintf("Internal issue: ret != 0 but errno is not set. Cancelling operation - %s ", C.GoString(&ans.errBuf[0]))
		}

		log.Warnf(msg)
		return nil, errors.New(msg)
	}

	response := []byte(C.GoString(ans.buf))
	C.Vmci_FreeBuf(ans)

	err = unmarshalError(response)
	if err != nil && len(err.Error()) != 0 {
		return nil, err
	}
	// There was no error, so return the slice containing the json response
	return response, nil
}
