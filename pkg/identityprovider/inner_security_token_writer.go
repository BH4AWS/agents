/*
Copyright 2026.

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

package identityprovider

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 used for file naming only, not security
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/process"
)

// RunCommandFunc is the function signature for executing a command inside a sandbox runtime.
// It mirrors the behavior of agentsruntime.RunCommandWithRuntime but is defined here to
// avoid circular imports (identityprovider → utils/runtime → sandbox-manager/config → identityprovider).
//
// Internal packages inject the real implementation via SetRunCommandFunc during init().
type RunCommandFunc func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, processConfig *process.ProcessConfig, timeout time.Duration) (stdout, stderr []string, exitCode int32, err error)

// runCommandFunc holds the injected RunCommandFunc implementation.
// It is nil by default (community mode). Internal deployments inject it via SetRunCommandFunc.
var runCommandFunc RunCommandFunc

// SetRunCommandFunc injects the runtime command execution function.
// This is called from utils/runtime init() to bridge the circular import gap.
func SetRunCommandFunc(fn RunCommandFunc) {
	runCommandFunc = fn
	klog.Info("identityprovider: RunCommandFunc injected")
}

// DefaultSecurityTokenFilePath is the default file path inside the sandbox runtime
// where the security token credential is written.
const DefaultSecurityTokenFilePath = "/var/opt/sandbox/agent-token/"

// defaultWriteTimeout is the timeout for the remote write command execution.
const defaultWriteTimeout = 10 * time.Second

// WriteSecurityTokenToRuntime is the SecurityTokenPropagator implementation that writes
// the issued security token into a credential file inside the sandbox runtime via RunCommand.
// It uses a shell script that:
//   - Creates the parent directory if it does not exist.
//   - If the target file already exists, logs a message and skips the write (no overwrite).
//   - If the target file does not exist, creates it and writes the token JSON content.
//
// This function is exported so that internal packages can register it via
// RegisterSecurityTokenPropagator during init(). Community code does not register it.
func WriteSecurityTokenToRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	if runCommandFunc == nil {
		log.Info("RunCommandFunc not injected, skipping security token write")
		return nil
	}

	if tokenResp == nil || tokenResp.AccessToken == "" {
		return fmt.Errorf("tokenResp is nil or AccessToken is empty, cannot write security token")
	}

	// File name: md5(<namespace>-<name>-<uid>).token
	identity := fmt.Sprintf("%s-%s-%s", sbx.Namespace, sbx.Name, sbx.UID)
	hash := fmt.Sprintf("%x", md5.Sum([]byte(identity))) //nolint:gosec // MD5 for file naming only
	filePath := fmt.Sprintf("%s%s.token", DefaultSecurityTokenFilePath, hash)

	// Write tokenResp.AccessToken as file content.
	tokenContent := tokenResp.AccessToken

	// Build a shell script that:
	// 1. Creates the parent directory (mkdir -p).
	// 2. Checks if the file already exists — if so, echo a message and exit 0 (skip).
	// 3. Otherwise, writes the token content to the file.
	//
	// Using heredoc (cat <<'TOKENEOF') to avoid shell escaping issues with JSON content.
	shellScript := fmt.Sprintf(
		`mkdir -p "$(dirname '%s')" && if [ -f '%s' ]; then echo "security token file already exists, skipping write"; else cat <<'TOKENEOF' > '%s'
%s
TOKENEOF
echo "security token written successfully"; fi`,
		filePath, filePath, filePath, tokenContent,
	)

	startTime := time.Now()
	log.Info("writing security token to runtime", "filePath", filePath)

	stdout, stderr, exitCode, err := runCommandFunc(ctx, sbx, &process.ProcessConfig{
		Cmd:  "/bin/sh",
		Args: []string{"-c", shellScript},
	}, defaultWriteTimeout)
	if err != nil {
		log.Error(err, "failed to execute security token write command",
			"stdout", stdout, "stderr", stderr)
		return fmt.Errorf("failed to write security token to runtime: %w", err)
	}
	if exitCode != 0 {
		err = fmt.Errorf("security token write command failed: [%d] %s", exitCode, strings.Join(stderr, ""))
		log.Error(err, "security token write command exited with non-zero code",
			"exitCode", exitCode, "stderr", stderr)
		return err
	}

	stdoutStr := strings.Join(stdout, "")
	if strings.Contains(stdoutStr, "already exists") {
		log.Info("security token file already exists in runtime, skipped overwrite",
			"filePath", filePath, "cost", time.Since(startTime))
	} else {
		log.Info("security token written to runtime successfully",
			"filePath", filePath, "cost", time.Since(startTime))
	}
	return nil
}
