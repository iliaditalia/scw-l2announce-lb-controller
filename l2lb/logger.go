/*
Copyright 2018 Scaleway
Copyright 2026 Iliad

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

package l2lb

import (
	"fmt"

	"github.com/scaleway/scaleway-sdk-go/logger"
	"k8s.io/klog/v2"
)

// Logger bridges the Scaleway SDK logger interface to klog.
// Copied from the Scaleway cloud-controller-manager (scaleway/logger.go).
type Logger struct{}

// Debugf logs to DEBUG log. Arguments are handled in the manner of fmt.Printf.
func (Logger) Debugf(format string, args ...any) {
	if klog.V(4).Enabled() {
		klog.InfoDepth(2, fmt.Sprintf(format, args...))
	}
}

// Infof logs to INFO log. Arguments are handled in the manner of fmt.Printf.
func (Logger) Infof(format string, args ...any) {
	if klog.V(2).Enabled() {
		klog.InfoDepth(2, fmt.Sprintf(format, args...))
	}
}

// Warningf logs to WARNING log. Arguments are handled in the manner of fmt.Printf.
func (Logger) Warningf(format string, args ...any) {
	klog.WarningDepth(2, fmt.Sprintf(format, args...))
}

// Errorf logs to ERROR log. Arguments are handled in the manner of fmt.Printf.
func (Logger) Errorf(format string, args ...any) {
	klog.ErrorDepth(2, fmt.Sprintf(format, args...))
}

// ShouldLog reports whether verbosity level l is at least the requested verbose level.
func (Logger) ShouldLog(level logger.LogLevel) bool {
	switch level {
	case logger.LogLevelError:
		return true
	case logger.LogLevelWarning:
		return true
	case logger.LogLevelInfo:
		if klog.V(2).Enabled() {
			return true
		}
	case logger.LogLevelDebug:
		if klog.V(5).Enabled() {
			return true
		}
	default:
		return true
	}

	return false
}
