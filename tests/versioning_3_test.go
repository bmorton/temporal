// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package tests

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dgryski/go-farm"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/suite"
	commandpb "go.temporal.io/api/command/v1"
	commonpb "go.temporal.io/api/common/v1"
	deploymentpb "go.temporal.io/api/deployment/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	deploymentspb "go.temporal.io/server/api/deployment/v1"
	"go.temporal.io/server/api/historyservice/v1"
	"go.temporal.io/server/api/matchingservice/v1"
	"go.temporal.io/server/common"
	"go.temporal.io/server/common/dynamicconfig"
	"go.temporal.io/server/common/payloads"
	"go.temporal.io/server/common/primitives/timestamp"
	"go.temporal.io/server/common/testing/taskpoller"
	"go.temporal.io/server/common/testing/testvars"
	"go.temporal.io/server/common/tqid"
	"go.temporal.io/server/tests/testcore"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	tqTypeWf        = enumspb.TASK_QUEUE_TYPE_WORKFLOW
	tqTypeAct       = enumspb.TASK_QUEUE_TYPE_ACTIVITY
	vbUnspecified   = enumspb.VERSIONING_BEHAVIOR_UNSPECIFIED
	vbPinned        = enumspb.VERSIONING_BEHAVIOR_PINNED
	vbUnpinned      = enumspb.VERSIONING_BEHAVIOR_AUTO_UPGRADE
	ver3MinPollTime = common.MinLongPollTimeout + time.Millisecond*200
)

type Versioning3Suite struct {
	// override suite.Suite.Assertions with require.Assertions; this means that s.NotNil(nil) will stop the test,
	// not merely log an error
	testcore.FunctionalTestBase
}

func TestVersioning3FunctionalSuite(t *testing.T) {
	t.Parallel()
	suite.Run(t, new(Versioning3Suite))
}

func (s *Versioning3Suite) SetupSuite() {
	dynamicConfigOverrides := map[dynamicconfig.Key]any{
		dynamicconfig.EnableDeployments.Key():                          true,
		dynamicconfig.FrontendEnableWorkerVersioningWorkflowAPIs.Key(): true,
		dynamicconfig.MatchingForwarderMaxChildrenPerNode.Key():        partitionTreeDegree,

		// Make sure we don't hit the rate limiter in tests
		dynamicconfig.FrontendGlobalNamespaceNamespaceReplicationInducingAPIsRPS.Key():                1000,
		dynamicconfig.FrontendMaxNamespaceNamespaceReplicationInducingAPIsBurstRatioPerInstance.Key(): 1,
		dynamicconfig.FrontendNamespaceReplicationInducingAPIsRPS.Key():                               1000,

		// this is overridden for tests using RunTestWithMatchingBehavior
		dynamicconfig.MatchingNumTaskqueueReadPartitions.Key():  4,
		dynamicconfig.MatchingNumTaskqueueWritePartitions.Key(): 4,
	}
	s.SetDynamicConfigOverrides(dynamicConfigOverrides)
	s.FunctionalTestBase.SetupSuite("testdata/es_cluster.yaml")
}

func (s *Versioning3Suite) TearDownSuite() {
	s.FunctionalTestBase.TearDownSuite()
}

func (s *Versioning3Suite) SetupTest() {
	s.FunctionalTestBase.SetupTest()
}

func (s *Versioning3Suite) TestPinnedTask_NoProperPoller() {
	s.RunTestWithMatchingBehavior(
		func() {
			tv := testvars.New(s)

			other := tv.WithBuildId("other")
			go s.idlePollWorkflow(other, true, ver3MinPollTime, "other deployment should not receive pinned task")
			s.waitForDeploymentDataPropagation(other, tqTypeWf)

			s.startWorkflow(tv, makePinnedOverride(tv.Deployment()))
			s.idlePollWorkflow(tv, false, ver3MinPollTime, "unversioned worker should not receive pinned task")
		})
}

func (s *Versioning3Suite) TestUnpinnedTask_NonCurrentDeployment() {
	s.RunTestWithMatchingBehavior(
		func() {
			tv := testvars.New(s)
			go s.idlePollWorkflow(tv, true, ver3MinPollTime, "non-current versioned poller should not receive unpinned task")
			s.waitForDeploymentDataPropagation(tv, tqTypeWf)

			s.startWorkflow(tv, nil)
		})
}

func (s *Versioning3Suite) TestUnpinnedTask_OldDeployment() {
	s.RunTestWithMatchingBehavior(
		func() {
			tv := testvars.New(s)
			// previous current deployment
			s.updateTaskQueueDeploymentData(tv.WithBuildId("older"), time.Minute, tqTypeWf)
			// current deployment
			s.updateTaskQueueDeploymentData(tv, 0, tqTypeWf)

			s.startWorkflow(tv, nil)

			s.idlePollWorkflow(
				tv.WithBuildId("older"),
				true,
				ver3MinPollTime,
				"old deployment should not receive unpinned task",
			)
		},
	)
}

func (s *Versioning3Suite) TestWorkflowWithPinnedOverride_Sticky() {
	s.RunTestWithMatchingBehavior(
		func() {
			s.testWorkflowWithPinnedOverride(true)
		},
	)
}

func (s *Versioning3Suite) TestWorkflowWithPinnedOverride_NoSticky() {
	s.RunTestWithMatchingBehavior(
		func() {
			s.testWorkflowWithPinnedOverride(false)
		},
	)
}

func (s *Versioning3Suite) testWorkflowWithPinnedOverride(sticky bool) {
	tv := testvars.New(s)

	if sticky {
		s.warmUpSticky(tv)
	}

	wftCompleted := make(chan interface{})
	s.pollWftAndHandle(tv, false, wftCompleted,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondWftWithActivities(tv, sticky, vbUnpinned, "5"), nil
		})
	s.waitForDeploymentDataPropagation(tv, tqTypeWf)

	actCompleted := make(chan interface{})
	s.pollActivityAndHandle(tv, actCompleted,
		func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondActivity(), nil
		})
	s.waitForDeploymentDataPropagation(tv, tqTypeAct)

	override := makePinnedOverride(tv.Deployment())
	we := s.startWorkflow(tv, override)

	<-wftCompleted
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), override, nil)
	if sticky {
		s.verifyWorkflowStickyQueue(we, tv.StickyTaskQueue())
	}

	<-actCompleted
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), override, nil)

	s.pollWftAndHandle(tv, sticky, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondCompleteWorkflow(tv, vbUnpinned), nil
		})
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), override, nil)
}

func (s *Versioning3Suite) TestUnpinnedWorkflow_Sticky() {
	s.RunTestWithMatchingBehavior(
		func() {
			s.testUnpinnedWorkflow(true)
		},
	)
}

func (s *Versioning3Suite) TestUnpinnedWorkflow_NoSticky() {
	s.RunTestWithMatchingBehavior(
		func() {
			s.testUnpinnedWorkflow(false)
		},
	)
}

func (s *Versioning3Suite) testUnpinnedWorkflow(sticky bool) {
	tv := testvars.New(s)
	d := tv.Deployment()

	if sticky {
		s.warmUpSticky(tv)
	}

	wftCompleted := make(chan interface{})
	s.pollWftAndHandle(tv, false, wftCompleted,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			s.verifyWorkflowVersioning(tv, vbUnspecified, nil, nil, transitionTo(d))
			return respondWftWithActivities(tv, sticky, vbUnpinned, "5"), nil
		})
	s.waitForDeploymentDataPropagation(tv, tqTypeWf)

	actCompleted := make(chan interface{})
	s.pollActivityAndHandle(tv, actCompleted,
		func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondActivity(), nil
		})
	s.waitForDeploymentDataPropagation(tv, tqTypeAct)

	s.updateTaskQueueDeploymentData(tv, 0, tqTypeWf, tqTypeAct)

	we := s.startWorkflow(tv, nil)

	<-wftCompleted
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), nil, nil)
	if sticky {
		s.verifyWorkflowStickyQueue(we, tv.StickyTaskQueue())
	}

	<-actCompleted
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), nil, nil)

	s.pollWftAndHandle(tv, sticky, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondCompleteWorkflow(tv, vbUnpinned), nil
		})
	s.verifyWorkflowVersioning(tv, vbUnpinned, tv.Deployment(), nil, nil)
}

func (s *Versioning3Suite) TestTransitionFromWft_Sticky() {
	s.testTransitionFromWft(true)
}

func (s *Versioning3Suite) TestTransitionFromWft_NoSticky() {
	s.testTransitionFromWft(false)
}

func (s *Versioning3Suite) testTransitionFromWft(sticky bool) {
	// Wf runs one TWF and one AC on dA, then the second WFT is redirected to dB and
	// transitions the wf with it.

	tvA := testvars.New(s).WithBuildId("A")
	tvB := tvA.WithBuildId("B")
	dA := tvA.Deployment()
	dB := tvB.Deployment()

	if sticky {
		s.warmUpSticky(tvA)
	}

	s.updateTaskQueueDeploymentData(tvA, 0, tqTypeWf, tqTypeAct)
	we := s.startWorkflow(tvA, nil)

	s.pollWftAndHandle(tvA, false, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			s.verifyWorkflowVersioning(tvA, vbUnspecified, nil, nil, transitionTo(dA))
			return respondWftWithActivities(tvA, sticky, vbUnpinned, "5"), nil
		})
	s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, nil)
	if sticky {
		s.verifyWorkflowStickyQueue(we, tvA.StickyTaskQueue())
	}

	s.pollActivityAndHandle(tvA, nil,
		func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondActivity(), nil
		})
	s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, nil)

	// Set B as the current deployment
	s.updateTaskQueueDeploymentData(tvB, 0, tqTypeWf, tqTypeAct)

	s.pollWftAndHandle(tvB, false, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, transitionTo(dB))
			return respondCompleteWorkflow(tvB, vbUnpinned), nil
		})
	s.verifyWorkflowVersioning(tvB, vbUnpinned, dB, nil, nil)
}

func (s *Versioning3Suite) TestTransitionFromActivity_Sticky() {
	s.testTransitionFromActivity(true)
}

func (s *Versioning3Suite) TestTransitionFromActivity_NoSticky() {
	s.testTransitionFromActivity(false)
}

func (s *Versioning3Suite) testTransitionFromActivity(sticky bool) {
	// Wf runs one TWF on dA and schedules four activities, then:
	// 1. The first and second activities starts on dA
	// 2. Current deployment becomes dB
	// 3. The third activity is redirected to dB and starts a transition in the wf, without being
	//    dispatched.
	// 4. The 4th activity also does not start on any of the builds although there are pending
	//    pollers on both.
	// 5. The transition generates a WFT and it is started in dB.
	// 6. The 1st act is completed here while the transition is going on.
	// 7. The 2nd act fails and makes another attempt. But it is not dispatched.
	// 8. WFT completes and the transition completes.
	// 9. All the 3 remaining activities are now dispatched and completed.

	tvA := testvars.New(s).WithBuildId("A")
	tvB := tvA.WithBuildId("B")
	dA := tvA.Deployment()
	dB := tvB.Deployment()

	if sticky {
		s.warmUpSticky(tvA)
	}

	s.updateTaskQueueDeploymentData(tvA, 0, tqTypeWf, tqTypeAct)
	we := s.startWorkflow(tvA, nil)

	s.pollWftAndHandle(tvA, false, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			s.verifyWorkflowVersioning(tvA, vbUnspecified, nil, nil, transitionTo(dA))
			return respondWftWithActivities(tvA, sticky, vbUnpinned, "5", "6", "7", "8"), nil
		})
	s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, nil)
	if sticky {
		s.verifyWorkflowStickyQueue(we, tvA.StickyTaskQueue())
	}

	// Set B as the current deployment
	s.updateTaskQueueDeploymentData(tvB, 0, tqTypeWf, tqTypeAct)

	// The poller should be present to the activity task is redirected, but it should not receive a
	// task until transition completes in the next wft.
	transitionCompleted := atomic.Bool{}
	actCompleted := make(chan interface{})
	s.pollActivityAndHandle(tvB, actCompleted,
		func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error) {
			// Activity should not start until the transition is completed
			s.True(transitionCompleted.Load())
			s.NotNil(task)
			return respondActivity(), nil
		})
	s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, nil)

	// The transition should create a new WFT to be sent to dB. Poller responds with empty wft complete.
	s.pollWftAndHandle(tvB, false, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			s.verifyWorkflowVersioning(tvA, vbUnpinned, dA, nil, transitionTo(dB))
			transitionCompleted.Store(true)
			return respondEmptyWft(tvB, sticky, vbUnpinned), nil
		})
	s.verifyWorkflowVersioning(tvB, vbUnpinned, dB, nil, nil)
	if sticky {
		s.verifyWorkflowStickyQueue(we, tvB.StickyTaskQueue())
	}

	s.pollWftAndHandle(tvB, false, nil,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.NotNil(task)
			return respondCompleteWorkflow(tvB, vbUnpinned), nil
		})
	s.verifyWorkflowVersioning(tvB, vbUnpinned, dB, nil, nil)
}

func transitionTo(d *deploymentpb.Deployment) *workflow.DeploymentTransition {
	return &workflow.DeploymentTransition{
		Deployment: d,
	}
}

func (s *Versioning3Suite) updateTaskQueueDeploymentData(
	tv *testvars.TestVars,
	timeSinceCurrent time.Duration,
	tqTypes ...enumspb.TaskQueueType,
) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	lastBecameCurrent := time.Now().Add(-timeSinceCurrent)
	for _, t := range tqTypes {
		_, err := s.GetTestCluster().MatchingClient().SyncDeploymentUserData(
			ctx, &matchingservice.SyncDeploymentUserDataRequest{
				NamespaceId:   s.GetNamespaceID(s.Namespace()),
				TaskQueue:     tv.TaskQueue().GetName(),
				TaskQueueType: t,
				Deployment:    tv.Deployment(),
				Data: &deploymentspb.TaskQueueData{
					FirstPollerTime:       timestamp.TimePtr(lastBecameCurrent),
					LastBecameCurrentTime: timestamp.TimePtr(lastBecameCurrent),
				},
			},
		)
		s.NoError(err)
	}
	s.waitForDeploymentDataPropagation(tv, tqTypes...)
}

func (s *Versioning3Suite) verifyWorkflowVersioning(
	tv *testvars.TestVars,
	behavior enumspb.VersioningBehavior,
	deployment *deploymentpb.Deployment,
	override *workflow.VersioningOverride,
	transition *workflow.DeploymentTransition,
) {
	dwf, err := s.FrontendClient().DescribeWorkflowExecution(
		context.Background(), &workflowservice.DescribeWorkflowExecutionRequest{
			Namespace: s.Namespace(),
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: tv.WorkflowID(),
			},
		},
	)
	s.NoError(err)

	versioningInfo := dwf.WorkflowExecutionInfo.GetVersioningInfo()
	s.Equal(behavior.String(), versioningInfo.GetBehavior().String())
	if !deployment.Equal(versioningInfo.GetDeployment()) {
		s.Fail(fmt.Sprintf("deployment mismatch. expected: {%s}, actual: {%s}",
			deployment,
			versioningInfo.GetDeployment(),
		))
	}

	s.Equal(override.GetBehavior().String(), versioningInfo.GetVersioningOverride().GetBehavior().String())
	if actualOverrideDeployment := versioningInfo.GetVersioningOverride().GetDeployment(); !override.GetDeployment().Equal(actualOverrideDeployment) {
		s.Fail(fmt.Sprintf("deployment override mismatch. expected: {%s}, actual: {%s}",
			override.GetDeployment(),
			actualOverrideDeployment,
		))
	}

	if !versioningInfo.GetDeploymentTransition().Equal(transition) {
		s.Fail(fmt.Sprintf("deployment override mismatch. expected: {%s}, actual: {%s}",
			transition,
			versioningInfo.GetDeploymentTransition(),
		))
	}
}

func respondActivity() *workflowservice.RespondActivityTaskCompletedRequest {
	return &workflowservice.RespondActivityTaskCompletedRequest{}
}

func respondWftWithActivities(
	tv *testvars.TestVars,
	sticky bool,
	behavior enumspb.VersioningBehavior,
	activityIds ...string,
) *workflowservice.RespondWorkflowTaskCompletedRequest {
	var stickyAttr *taskqueuepb.StickyExecutionAttributes
	if sticky {
		stickyAttr = &taskqueuepb.StickyExecutionAttributes{
			WorkerTaskQueue:        tv.StickyTaskQueue(),
			ScheduleToStartTimeout: durationpb.New(5 * time.Second),
		}
	}
	var commands []*commandpb.Command
	for _, a := range activityIds {
		commands = append(commands, &commandpb.Command{
			CommandType: enumspb.COMMAND_TYPE_SCHEDULE_ACTIVITY_TASK,
			Attributes: &commandpb.Command_ScheduleActivityTaskCommandAttributes{
				ScheduleActivityTaskCommandAttributes: &commandpb.ScheduleActivityTaskCommandAttributes{
					ActivityId:   a,
					ActivityType: &commonpb.ActivityType{Name: "act"},
					TaskQueue:    tv.TaskQueue(),
					Input:        payloads.EncodeString("input"),
					// TODO (shahab): tests with forced task forward take multiple seconds. Need to know why?
					ScheduleToCloseTimeout: durationpb.New(10 * time.Second),
					ScheduleToStartTimeout: durationpb.New(10 * time.Second),
					StartToCloseTimeout:    durationpb.New(1 * time.Second),
					HeartbeatTimeout:       durationpb.New(1 * time.Second),
					RequestEagerExecution:  false,
				},
			},
		})
	}
	return &workflowservice.RespondWorkflowTaskCompletedRequest{
		Commands:                   commands,
		StickyAttributes:           stickyAttr,
		ReturnNewWorkflowTask:      false,
		ForceCreateNewWorkflowTask: false,
		Deployment:                 tv.Deployment(),
		VersioningBehavior:         behavior,
	}
}

func respondEmptyWft(
	tv *testvars.TestVars,
	sticky bool,
	behavior enumspb.VersioningBehavior,
) *workflowservice.RespondWorkflowTaskCompletedRequest {
	var stickyAttr *taskqueuepb.StickyExecutionAttributes
	if sticky {
		stickyAttr = &taskqueuepb.StickyExecutionAttributes{
			WorkerTaskQueue:        tv.StickyTaskQueue(),
			ScheduleToStartTimeout: durationpb.New(5 * time.Second),
		}
	}
	return &workflowservice.RespondWorkflowTaskCompletedRequest{
		StickyAttributes:           stickyAttr,
		ReturnNewWorkflowTask:      false,
		ForceCreateNewWorkflowTask: false,
		Deployment:                 tv.Deployment(),
		VersioningBehavior:         behavior,
	}
}

func respondCompleteWorkflow(
	tv *testvars.TestVars,
	behavior enumspb.VersioningBehavior,
) *workflowservice.RespondWorkflowTaskCompletedRequest {
	return &workflowservice.RespondWorkflowTaskCompletedRequest{
		Commands: []*commandpb.Command{
			{
				CommandType: enumspb.COMMAND_TYPE_COMPLETE_WORKFLOW_EXECUTION,
				Attributes: &commandpb.Command_CompleteWorkflowExecutionCommandAttributes{
					CompleteWorkflowExecutionCommandAttributes: &commandpb.CompleteWorkflowExecutionCommandAttributes{
						Result: payloads.EncodeString("done"),
					},
				},
			},
		},
		ReturnNewWorkflowTask:      false,
		ForceCreateNewWorkflowTask: false,
		Deployment:                 tv.Deployment(),
		VersioningBehavior:         behavior,
	}
}

func (s *Versioning3Suite) startWorkflow(
	tv *testvars.TestVars,
	override *workflow.VersioningOverride,
) *commonpb.WorkflowExecution {
	id := tv.WorkflowID()
	wt := "MyWfType"
	identity := "worker1"

	request := &workflowservice.StartWorkflowExecutionRequest{
		RequestId:           uuid.New(),
		Namespace:           s.Namespace(),
		WorkflowId:          id,
		WorkflowType:        &commonpb.WorkflowType{Name: wt},
		TaskQueue:           tv.TaskQueue(),
		Input:               nil,
		WorkflowRunTimeout:  durationpb.New(100 * time.Second),
		WorkflowTaskTimeout: durationpb.New(10 * time.Second),
		Identity:            identity,
		VersioningOverride:  override,
	}

	we, err0 := s.FrontendClient().StartWorkflowExecution(testcore.NewContext(), request)
	s.NoError(err0)
	return &commonpb.WorkflowExecution{
		WorkflowId: id,
		RunId:      we.GetRunId(),
	}
}

// Name is used by testvars. We use a shorten test name in variables so that physical task queue IDs
// do not grow larger that DB column limit (currently as low as 272 chars).
func (s *Versioning3Suite) Name() string {
	fullName := s.T().Name()
	if len(fullName) <= 30 {
		return fullName
	}
	return fmt.Sprintf("%s-%08x",
		fullName[len(fullName)-21:],
		farm.Fingerprint32([]byte(fullName)),
	)
}

// pollWftAndHandle can be used in sync and async mode. For async mode pass the async channel. It
// will be closed when the task is handled.
func (s *Versioning3Suite) pollWftAndHandle(
	tv *testvars.TestVars,
	sticky bool,
	async chan<- interface{},
	handler func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error),
) {
	poller := taskpoller.New(s.T(), s.FrontendClient(), s.Namespace())
	d := tv.Deployment()
	tq := tv.TaskQueue()
	if sticky {
		tq = tv.StickyTaskQueue()
	}
	f := func() {
		_, err := poller.PollWorkflowTask(
			&workflowservice.PollWorkflowTaskQueueRequest{
				WorkerVersionCapabilities: &commonpb.WorkerVersionCapabilities{
					BuildId:              d.BuildId,
					DeploymentSeriesName: d.SeriesName,
					UseVersioning:        true,
				},
				TaskQueue: tq,
			},
		).HandleTask(tv, handler)
		s.NoError(err)
	}
	if async == nil {
		f()
	} else {
		go func() {
			f()
			close(async)
		}()
	}
}

func (s *Versioning3Suite) pollActivityAndHandle(
	tv *testvars.TestVars,
	async chan<- interface{},
	handler func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error),
) {
	poller := taskpoller.New(s.T(), s.FrontendClient(), s.Namespace())
	d := tv.Deployment()
	f := func() {
		_, err := poller.PollActivityTask(
			&workflowservice.PollActivityTaskQueueRequest{
				WorkerVersionCapabilities: &commonpb.WorkerVersionCapabilities{
					BuildId:              d.BuildId,
					DeploymentSeriesName: d.SeriesName,
					UseVersioning:        true,
				},
			},
		).HandleTask(tv, handler, taskpoller.WithTimeout(time.Minute))
		s.NoError(err)
	}
	if async == nil {
		f()
	} else {
		go func() {
			f()
			close(async)
		}()
	}
}

func (s *Versioning3Suite) idlePollWorkflow(
	tv *testvars.TestVars,
	versioned bool,
	timeout time.Duration,
	unexpectedTaskMessage string,
) {
	poller := taskpoller.New(s.T(), s.FrontendClient(), s.Namespace())
	d := tv.Deployment()
	_, _ = poller.PollWorkflowTask(
		&workflowservice.PollWorkflowTaskQueueRequest{
			WorkerVersionCapabilities: &commonpb.WorkerVersionCapabilities{
				BuildId:              d.BuildId,
				DeploymentSeriesName: d.SeriesName,
				UseVersioning:        versioned,
			},
		},
	).HandleTask(
		tv,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.Fail(unexpectedTaskMessage)
			return nil, nil
		},
		taskpoller.WithTimeout(timeout),
	)
}

func (s *Versioning3Suite) idlePollActivity(
	tv *testvars.TestVars,
	versioned bool,
	timeout time.Duration,
	unexpectedTaskMessage string,
) {
	poller := taskpoller.New(s.T(), s.FrontendClient(), s.Namespace())
	d := tv.Deployment()
	_, _ = poller.PollActivityTask(
		&workflowservice.PollActivityTaskQueueRequest{
			WorkerVersionCapabilities: &commonpb.WorkerVersionCapabilities{
				BuildId:              d.BuildId,
				DeploymentSeriesName: d.SeriesName,
				UseVersioning:        versioned,
			},
		},
	).HandleTask(
		tv,
		func(task *workflowservice.PollActivityTaskQueueResponse) (*workflowservice.RespondActivityTaskCompletedRequest, error) {
			s.Fail(unexpectedTaskMessage)
			return nil, nil
		},
		taskpoller.WithTimeout(timeout),
	)
}

func (s *Versioning3Suite) verifyWorkflowStickyQueue(
	we *commonpb.WorkflowExecution,
	stickyQ *taskqueuepb.TaskQueue,
) {
	ms, err := s.GetTestCluster().HistoryClient().GetMutableState(
		context.Background(), &historyservice.GetMutableStateRequest{
			NamespaceId: s.GetNamespaceID(s.Namespace()),
			Execution:   we,
		},
	)
	s.NoError(err)
	s.Equal(stickyQ.GetName(), ms.StickyTaskQueue.GetName())
}

// Sticky queue needs to be created in server before tasks can schedule in it. Call to this method
// create the sticky queue by polling it.
func (s *Versioning3Suite) warmUpSticky(
	tv *testvars.TestVars,
) {
	poller := taskpoller.New(s.T(), s.FrontendClient(), s.Namespace())
	_, _ = poller.PollWorkflowTask(
		&workflowservice.PollWorkflowTaskQueueRequest{
			TaskQueue: tv.StickyTaskQueue(),
		},
	).HandleTask(
		tv,
		func(task *workflowservice.PollWorkflowTaskQueueResponse) (*workflowservice.RespondWorkflowTaskCompletedRequest, error) {
			s.Fail("sticky task is not expected")
			return nil, nil
		},
		taskpoller.WithTimeout(ver3MinPollTime),
	)
}

func (s *Versioning3Suite) waitForDeploymentDataPropagation(
	tv *testvars.TestVars,
	tqTypes ...enumspb.TaskQueueType,
) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	v := s.GetTestCluster().Host().DcClient().GetValue(dynamicconfig.MatchingNumTaskqueueReadPartitions.Key())
	s.NotEmpty(v, "versioning tests require setting explicit number of partitions")
	count, ok := v[0].Value.(int)
	s.True(ok, "partition count is not an int")
	partitionCount := count

	type partAndType struct {
		part int
		tp   enumspb.TaskQueueType
	}
	remaining := make(map[partAndType]struct{})
	for i := 0; i < partitionCount; i++ {
		for _, tqt := range tqTypes {
			remaining[partAndType{i, tqt}] = struct{}{}
		}
	}
	nsId := s.GetNamespaceID(s.Namespace())
	f, err := tqid.NewTaskQueueFamily(nsId, tv.TaskQueue().GetName())
	deployment := tv.Deployment()
	s.Eventually(func() bool {
		for pt := range remaining {
			s.NoError(err)
			partition := f.TaskQueue(pt.tp).NormalPartition(pt.part)
			// Use lower-level GetTaskQueueUserData instead of GetWorkerBuildIdCompatibility
			// here so that we can target activity queues.
			res, err := s.GetTestCluster().MatchingClient().GetTaskQueueUserData(
				ctx,
				&matchingservice.GetTaskQueueUserDataRequest{
					NamespaceId:   nsId,
					TaskQueue:     partition.RpcName(),
					TaskQueueType: partition.TaskType(),
				})
			s.NoError(err)
			perTypes := res.GetUserData().GetData().GetPerType()
			if perTypes != nil {
				deps := perTypes[int32(pt.tp)].GetDeploymentData().GetDeployments()
				for _, d := range deps {
					if d.GetDeployment().Equal(deployment) {
						delete(remaining, pt)
					}
				}
			}
		}
		return len(remaining) == 0
	}, 10*time.Second, 100*time.Millisecond)
}

func makePinnedOverride(d *deploymentpb.Deployment) *workflow.VersioningOverride {
	return &workflow.VersioningOverride{Behavior: vbPinned, Deployment: d}
}
