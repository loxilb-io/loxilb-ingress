/*
 * Copyright (c) 2024 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pkg

import (
	"context"
	"os"
	"os/exec"
	"time"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	"k8s.io/klog/v2"
)

const (
	LoxiLBImg = "/root/loxilb-io/loxilb/loxilb"
)

func SpawnLoxiLB(enablePrometheus bool, proxyOnlyMode bool) {
	for {
		args := []string{}

		if proxyOnlyMode {
			args = append(args, "--proxyonlymode")
		}

		if enablePrometheus {
			args = append(args, "-p")
		}

		cmd := exec.Command(LoxiLBImg, args...)
		klog.Infof("Spawning loxilb: %s", cmd)
		cmd.Stdout = os.Stdout
		err := cmd.Run()
		if err != nil {
			klog.Infof("Spawning loxilb failed: %s", err)
			os.Exit(2)
		}
		time.Sleep(3000 * time.Millisecond)
	}
}

// WaitForLoxiLB polls loxilb's API every interval until it answers or ctx ends, and reports whether
// it answered. Any HTTP response counts. loxilb starts its API only after its datapath is set up,
// so once it answers, rules can be installed.
func WaitForLoxiLB(ctx context.Context, client *loxiapi.LoxiClient, interval time.Duration) bool {
	for {
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := client.HealthCheck().Get(reqCtx, "")
		cancel()
		if err == nil {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
}
