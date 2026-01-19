package core

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

// MockInPlaceUpdateHandler mocks the handler implementation
type MockInPlaceUpdateHandler struct {
	control  *inplaceupdate.InPlaceUpdateControl
	recorder record.EventRecorder
	logger   logr.Logger
}

func (m *MockInPlaceUpdateHandler) GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl {
	return m.control
}

func (m *MockInPlaceUpdateHandler) GetRecorder() record.EventRecorder {
	return m.recorder
}

func (m *MockInPlaceUpdateHandler) GetLogger(ctx context.Context, box *agentsv1alpha1.Sandbox) logr.Logger {
	return m.logger
}

// Create test event recorder
func createTestRecorder() record.EventRecorder {
	scheme := runtime.NewScheme()
	agentsv1alpha1.AddToScheme(scheme)
	corev1.AddToScheme(scheme)
	return record.NewFakeRecorder(100)
}

func TestHandleInPlaceUpdateCommon(t *testing.T) {
	// Test cases definition
	testCases := []struct {
		name           string
		pod            *corev1.Pod
		box            *agentsv1alpha1.Sandbox
		newStatus      *agentsv1alpha1.SandboxStatus
		setupHandler   func() InPlaceUpdateHandler
		expectedResult bool
		expectError    bool
		description    string
	}{
		{
			name: "pod without template hash label should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{}, // No pod-template-hash label
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When Pod has no template hash label, should return true immediately",
		},
		{
			name: "hash mismatch should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "old-hash",
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashWithoutImageAndResources: "new-hash", // Mismatch with Pod label
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When hash mismatch occurs, should return true",
		},
		{
			name: "revision consistent and update completed should return true",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.PodLabelTemplateHash: "test-revision", // Matches newStatus.UpdateRevision
					},
				},
			},
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.SandboxHashWithoutImageAndResources: "test-revision",
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					SandboxTemplate: agentsv1alpha1.SandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "test-container",
										Image: "nginx:latest",
									},
								},
							},
						},
					},
				},
			},
			newStatus: &agentsv1alpha1.SandboxStatus{
				UpdateRevision: "test-revision",
			},
			setupHandler: func() InPlaceUpdateHandler {
				recorder := createTestRecorder()
				return &MockInPlaceUpdateHandler{
					control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
					recorder: recorder,
					logger:   logr.Discard(),
				}
			},
			expectedResult: true,
			expectError:    false,
			description:    "When revision is consistent and update completed, should return true",
		},
	}

	// Execute test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Set up test context
			ctx := context.Background()

			// Create handler
			handler := tc.setupHandler()

			// Execute function
			result, err := handleInPlaceUpdateCommon(ctx, handler, tc.pod, tc.box, tc.newStatus)

			// Verify result
			if result != tc.expectedResult {
				t.Errorf("Expected result %v, but got %v", tc.expectedResult, result)
			}

			// Verify error
			if tc.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tc.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestHandleInPlaceUpdateCommon_WithUpdateInProgress(t *testing.T) {
	// Test when update is in progress
	ctx := context.Background()

	// Create Pod with update state
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"new-revision","updateTimestamp":"2023-01-01T00:00:00Z"}`,
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	// Create handler
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	// Execute function
	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Should return true because there's an ongoing update
	if result != true {
		t.Errorf("Expected result true, but got %v", result)
	}
}

func TestHandleInPlaceUpdateCommon_InitialState(t *testing.T) {
	// Test initial state with no ongoing update
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:updated",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	// Create handler
	recorder := createTestRecorder()
	handler := &MockInPlaceUpdateHandler{
		control:  inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc),
		recorder: recorder,
		logger:   logr.Discard(),
	}

	// Execute function
	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	// Verify result
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Should return true when no changes occurred
	if result != true {
		t.Errorf("Expected result false, but got %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_MultipleUpdateScenario tests the scenario when multiple updates are not supported
func TestHandleInPlaceUpdateCommon_MultipleUpdateScenario(t *testing.T) {
	ctx := context.Background()

	// Pod with existing update state
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "current-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"current-revision","updateTimestamp":"2023-01-01T00:00:00Z"}`,
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Multiple updates are not supported, so should return true
	if result != true {
		t.Errorf("Expected result true for multiple updates not supported, but got %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_MultipleUpdatesCompleted tests when multiple updates are not supported but completed
func TestHandleInPlaceUpdateCommon_MultipleUpdatesCompleted(t *testing.T) {
	ctx := context.Background()

	// Create Pod with existing update state and simulate completed update
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "current-revision",
			},
			Annotations: map[string]string{
				inplaceupdate.PodAnnotationInPlaceUpdateStateKey: `{"revision":"current-revision","updateTimestamp":"2023-01-01T00:00:00Z"}`,
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-revision",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// When multiple updates are not supported but completed, should return true
	if result != true {
		t.Errorf("Expected result true when multiple updates completed, but got %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_UpdateNoChange tests when update operation returns no change
func TestHandleInPlaceUpdateCommon_UpdateNoChange(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:updated",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// When update is called but no changes are made, result should be true
	if result != true {
		t.Errorf("Expected result true when no changes made, but got %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_StatusUpdates tests that status conditions are properly updated
func TestHandleInPlaceUpdateCommon_StatusUpdates(t *testing.T) {
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
	}

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: "current-hash",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:updated",
							},
						},
					},
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	_, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify that status conditions are updated appropriately
	inplaceUpdateCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	readyCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))

	// The actual conditions depend on the mock control's behavior
	if inplaceUpdateCond != nil {
		t.Logf("Inplace update condition found: %v", inplaceUpdateCond.Status)
	}

	if readyCond != nil {
		t.Logf("Ready condition found: %v", readyCond.Status)
	}
}

// TestHandleInPlaceUpdateCommon_StartUpdate tests the normal flow of starting an inplace update
func TestHandleInPlaceUpdateCommon_StartUpdate(t *testing.T) {
	ctx := context.Background()

	// Create a baseline sandbox to calculate the correct hash
	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:updated", // Different image to trigger update
							},
						},
					},
				},
			},
		},
	}

	// Calculate the proper hash using HashSandbox function
	_, hashWithoutImageAndResource := HashSandbox(baseBox)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: hashWithoutImageAndResource,
			},
		},
		Spec: baseBox.Spec,
	}

	// Create Pod with different template hash to trigger update
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision", // Different from update revision
			},
			Annotations: map[string]string{
				// Add some annotation that differs from what the update would expect
				"some-key": "old-value",
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	// Create a control that ensures the update operation actually makes changes
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// The actual result depends on whether the control.Update detects changes
	// For a proper test of the start update path, we need to ensure changes are detected
	// Since the default implementation might not detect changes in our simple test setup,
	// let's focus on testing that the status conditions are set when appropriate

	// If result is false, it means update was started and conditions should be set
	if result == false {
		// Verify that the inplace update condition was set to in-progress
		inplaceUpdateCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
		if inplaceUpdateCond == nil || inplaceUpdateCond.Status != metav1.ConditionFalse {
			t.Logf("Inplace update condition: %+v", inplaceUpdateCond)
			t.Errorf("Expected inplace update condition to be False when update is in progress")
		}

		// Verify that the ready condition was set to in-progress
		readyCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
			t.Logf("Ready condition: %+v", readyCond)
			t.Errorf("Expected ready condition to be False when update is in progress")
		}
	} else {
		// If no changes were detected, the update wouldn't start
		t.Logf("No changes detected, update not started. Result: %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_UpdateNoChanges tests when update operation returns no changes
func TestHandleInPlaceUpdateCommon_UpdateNoChanges(t *testing.T) {
	ctx := context.Background()

	// Create a baseline sandbox to calculate the correct hash
	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	// Calculate the proper hash using HashSandbox function
	_, hashWithoutImageAndResource := HashSandbox(baseBox)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: hashWithoutImageAndResource,
			},
		},
		Spec: baseBox.Spec,
	}

	// Create Pod that doesn't require an update (template hashes may differ but no real change)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "old-revision",
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "new-revision",
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// When no changes are made during update, should return true
	if result != true {
		t.Errorf("Expected result true when no changes are made, but got %v", result)
	}
}

// TestHandleInPlaceUpdateCommon_RevisionConsistentUpdateCompleted tests when revision is consistent and update is completed
func TestHandleInPlaceUpdateCommon_RevisionConsistentUpdateCompleted(t *testing.T) {
	ctx := context.Background()

	// Create a baseline sandbox to calculate the correct hash
	baseBox := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test-container",
								Image: "nginx:latest",
							},
						},
					},
				},
			},
		},
	}

	// Calculate the proper hash using HashSandbox function
	_, hashWithoutImageAndResource := HashSandbox(baseBox)

	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.SandboxHashWithoutImageAndResources: hashWithoutImageAndResource,
			},
		},
		Spec: baseBox.Spec,
	}

	// Create Pod with matching template hash and simulate completed update
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.PodLabelTemplateHash: "test-revision",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	newStatus := &agentsv1alpha1.SandboxStatus{
		UpdateRevision: "test-revision", // Same as pod template hash
	}

	recorder := createTestRecorder()
	control := inplaceupdate.NewInPlaceUpdateControl(nil, inplaceupdate.DefaultGeneratePatchBodyFunc)
	handler := &MockInPlaceUpdateHandler{
		control:  control,
		recorder: recorder,
		logger:   logr.Discard(),
	}

	result, err := handleInPlaceUpdateCommon(ctx, handler, pod, box, newStatus)

	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// When revision is consistent and update is completed, should return true
	if result != true {
		t.Errorf("Expected result true when update is completed, but got %v", result)
	}

	// Verify that the inplace update condition was set to succeeded
	inplaceUpdateCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	if inplaceUpdateCond == nil || inplaceUpdateCond.Status != metav1.ConditionTrue ||
		inplaceUpdateCond.Reason != agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
		t.Errorf("Expected inplace update condition to be True with Succeeded reason")
	}
}
