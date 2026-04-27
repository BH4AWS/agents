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
	"crypto/md5" //nolint:gosec // MD5 used for file naming only in tests
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/process"
)

// ---------------------------------------------------------------------------
// SetRunCommandFunc
// ---------------------------------------------------------------------------

func TestSetRunCommandFunc(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	assert.Nil(t, runCommandFunc, "should be nil by default")

	fn := func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return nil, nil, 0, nil
	}
	SetRunCommandFunc(fn)
	assert.NotNil(t, runCommandFunc, "should be set after injection")
}

// ---------------------------------------------------------------------------
// WriteSecurityTokenToRuntime
// ---------------------------------------------------------------------------

func newTestSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sbx",
			Namespace: "default",
			UID:       types.UID("uid-123"),
		},
	}
}

func expectedTokenFilePath(sbx *agentsv1alpha1.Sandbox) string {
	identity := fmt.Sprintf("%s-%s-%s", sbx.Namespace, sbx.Name, sbx.UID)
	hash := fmt.Sprintf("%x", md5.Sum([]byte(identity))) //nolint:gosec
	return fmt.Sprintf("%s%s.token", DefaultSecurityTokenFilePath, hash)
}

func TestWriteSecurityTokenToRuntime_NilRunCommandFunc(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()
	runCommandFunc = nil

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), &TokenResponse{AccessToken: "tok"})
	assert.NoError(t, err, "should skip gracefully when RunCommandFunc is nil")
}

func TestWriteSecurityTokenToRuntime_NilTokenResp(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()
	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return nil, nil, 0, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenResp is nil or AccessToken is empty")
}

func TestWriteSecurityTokenToRuntime_EmptyAccessToken(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()
	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return nil, nil, 0, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), &TokenResponse{AccessToken: ""})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenResp is nil or AccessToken is empty")
}

func TestWriteSecurityTokenToRuntime_Success(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	sbx := newTestSandbox()
	expectedPath := expectedTokenFilePath(sbx)
	var capturedScript string

	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, cfg *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		assert.Equal(t, "/bin/sh", cfg.Cmd)
		require.Len(t, cfg.Args, 2)
		capturedScript = cfg.Args[1]
		return []string{"security token written successfully"}, nil, 0, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), sbx, &TokenResponse{AccessToken: "my-access-token"})
	require.NoError(t, err)

	// Verify the shell script contains the expected file path and token content.
	assert.Contains(t, capturedScript, expectedPath)
	assert.Contains(t, capturedScript, "my-access-token")
}

func TestWriteSecurityTokenToRuntime_AlreadyExists(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return []string{"security token file already exists, skipping write"}, nil, 0, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), &TokenResponse{AccessToken: "tok"})
	assert.NoError(t, err, "already exists should not return error")
}

func TestWriteSecurityTokenToRuntime_CommandError(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return nil, nil, 0, fmt.Errorf("connection refused")
	}

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), &TokenResponse{AccessToken: "tok"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to write security token to runtime")
}

func TestWriteSecurityTokenToRuntime_NonZeroExitCode(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		return nil, []string{"permission denied"}, 1, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), newTestSandbox(), &TokenResponse{AccessToken: "tok"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "security token write command failed")
	assert.Contains(t, err.Error(), "permission denied")
}

func TestWriteSecurityTokenToRuntime_FilePathIsMD5Based(t *testing.T) {
	saved := runCommandFunc
	defer func() { runCommandFunc = saved }()

	sbx := newTestSandbox()
	expectedPath := expectedTokenFilePath(sbx)

	var capturedScript string
	runCommandFunc = func(_ context.Context, _ *agentsv1alpha1.Sandbox, cfg *process.ProcessConfig, _ time.Duration) ([]string, []string, int32, error) {
		capturedScript = cfg.Args[1]
		return []string{"security token written successfully"}, nil, 0, nil
	}

	err := WriteSecurityTokenToRuntime(context.Background(), sbx, &TokenResponse{AccessToken: "tok"})
	require.NoError(t, err)
	assert.Contains(t, capturedScript, expectedPath, "file path should be MD5 hash based with .token extension")
}
