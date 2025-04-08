package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/core"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/metrics"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisconnect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/disconnect":
			rw.WriteHeader(http.StatusOK)
			fmt.Fprintf(rw, `{"id": "fakeuuid", "connection_state": "disconnected"}`)
		default:
			t.Errorf("Unknown endpoint %s %s", req.Method, req.URL.Path)
			http.Error(rw, "Not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	l := logger.NewBuffer()
	client := &core.Client{
		APIClient: apiClient,
		Logger:    l,
		RetrySleepFunc: func(time.Duration) {
			t.Error("unexpected retrier sleep")
		},
	}

	worker := &AgentWorker{
		logger:             l,
		agent:              nil,
		apiClient:          apiClient,
		client:             client,
		agentConfiguration: AgentConfiguration{},
	}

	err := worker.Disconnect(ctx)
	require.NoError(t, err)

	assert.Equal(t, []string{"[info] Disconnecting...", "[info] Disconnected"}, l.Messages)
}

func TestDisconnectRetry(t *testing.T) {
	t.Parallel()

	tries := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/disconnect":
			if tries < 2 { // three failures before success
				rw.WriteHeader(http.StatusInternalServerError)
				tries++
			} else {
				rw.WriteHeader(http.StatusOK)
				fmt.Fprintf(rw, `{"id": "fakeuuid", "connection_state": "disconnected"}`)
			}
		default:
			t.Errorf("Unknown endpoint %s %s", req.Method, req.URL.Path)
			http.Error(rw, "Not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx := context.Background()

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	l := logger.NewBuffer()
	retrySleeps := make([]time.Duration, 0)
	retrySleepFunc := func(d time.Duration) {
		retrySleeps = append(retrySleeps, d)
	}
	client := &core.Client{
		APIClient:      apiClient,
		Logger:         l,
		RetrySleepFunc: retrySleepFunc,
	}

	worker := &AgentWorker{
		logger:             l,
		agent:              nil,
		apiClient:          apiClient,
		client:             client,
		agentConfiguration: AgentConfiguration{},
	}

	err := worker.Disconnect(ctx)
	assert.NoError(t, err)

	// 2 failed attempts sleep 1 second each
	assert.Equal(t, []time.Duration{1 * time.Second, 1 * time.Second}, retrySleeps)

	require.Equal(t, 4, len(l.Messages))
	assert.Equal(t, "[info] Disconnecting...", l.Messages[0])
	assert.Regexp(t, regexp.MustCompile(`\[warn\] POST http.*/disconnect: 500 Internal Server Error \(Attempt 1/4`), l.Messages[1])
	assert.Regexp(t, regexp.MustCompile(`\[warn\] POST http.*/disconnect: 500 Internal Server Error \(Attempt 2/4`), l.Messages[2])
	assert.Equal(t, "[info] Disconnected", l.Messages[3])
}

func TestAcquireJobReturnsWrappedError_WhenServerResponds422(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	jobID := "some-uuid"

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case fmt.Sprintf("/jobs/%s/acquire", jobID):
			rw.WriteHeader(http.StatusUnprocessableEntity)
			return

		default:
			http.Error(rw, "Not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	worker := &AgentWorker{
		logger:    logger.Discard,
		agent:     nil,
		apiClient: apiClient,
		client: &core.Client{
			APIClient: apiClient,
			Logger:    logger.Discard,
		},
		agentConfiguration: AgentConfiguration{},
	}

	err := worker.AcquireAndRunJob(ctx, jobID)
	if !errors.Is(err, core.ErrJobAcquisitionRejected) {
		t.Fatalf("expected worker.AcquireAndRunJob(%q) = core.ErrJobAcquisitionRejected, got %v", jobID, err)
	}
}

func TestAcquireAndRunJobWaiting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/jobs/waitinguuid/acquire":
			if req.Header.Get("X-Buildkite-Lock-Acquire-Job") != "1" {
				http.Error(rw, "Expected X-Buildkite-Lock-Acquire-Job to be set to 1", http.StatusUnprocessableEntity)
				return
			}

			backoff_seq, err := strconv.ParseFloat(req.Header.Get("X-Buildkite-Backoff-Sequence"), 64)
			if err != nil {
				backoff_seq = 0
			}
			delay := math.Pow(2, backoff_seq)

			rw.Header().Set("Retry-After", fmt.Sprintf("%f", delay))
			rw.WriteHeader(http.StatusLocked)
			fmt.Fprintf(rw, `{"message": "Job waitinguuid is not yet eligible to be assigned"}`)
		default:
			http.Error(rw, "Not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	retrySleeps := []time.Duration{}
	retrySleepFunc := func(d time.Duration) {
		retrySleeps = append(retrySleeps, d)
	}
	client := &core.Client{
		APIClient:      apiClient,
		Logger:         logger.Discard,
		RetrySleepFunc: retrySleepFunc,
	}

	worker := &AgentWorker{
		logger:             logger.Discard,
		agent:              nil,
		apiClient:          apiClient,
		client:             client,
		agentConfiguration: AgentConfiguration{},
	}

	err := worker.AcquireAndRunJob(ctx, "waitinguuid")
	assert.ErrorContains(t, err, "423")

	if errors.Is(err, core.ErrJobAcquisitionRejected) {
		t.Fatalf("expected worker.AcquireAndRunJob(%q) not to be core.ErrJobAcquisitionRejected, but it was: %v", "waitinguuid", err)
	}

	// the last Retry-After is not recorded as the retries loop exits before using it
	expectedSleeps := make([]time.Duration, 0, 6)
	for d := 1; d <= 1<<5; d *= 2 {
		expectedSleeps = append(expectedSleeps, time.Duration(d)*time.Second)
	}
	assert.Equal(t, expectedSleeps, retrySleeps)
}

func TestAgentWorker_Start_AcquireJob_Pause_Unpause(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := NewFakeAPIServer()
	defer server.Close()

	job := server.AddJob(map[string]string{
		"BUILDKITE_COMMAND": "echo echo",
	})

	// Pre-register the agent.
	const agentSessionToken = "alpacas"
	agent := server.AddAgent(agentSessionToken)
	agent.PingHandler = func() (api.Ping, error) {
		switch agent.Pings {
		case 0:
			return api.Ping{
				Action:  "pause",
				Message: "Agent is now paused",
			}, nil

		case 1:
			return api.Ping{}, nil // now idle

		default:
			return api.Ping{}, errors.New("too many pings")
		}
	}

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	l := logger.NewConsoleLogger(logger.NewTestPrinter(t), func(int) {})

	worker := NewAgentWorker(
		l,
		&api.AgentRegisterResponse{
			UUID:              uuid.New().String(),
			Name:              "agent-1",
			AccessToken:       agentSessionToken,
			Endpoint:          server.URL,
			PingInterval:      1,
			JobStatusInterval: 1,
			HeartbeatInterval: 10,
		},
		metrics.NewCollector(logger.Discard, metrics.CollectorConfig{}),
		apiClient,
		AgentWorkerConfig{
			SpawnIndex: 1,
			AgentConfiguration: AgentConfiguration{
				BootstrapScript: "./dummy_bootstrap.sh",
				BuildPath:       filepath.Join(os.TempDir(), t.Name(), "build"),
				HooksPath:       filepath.Join(os.TempDir(), t.Name(), "hooks"),
				AcquireJob:      job.Job.ID,
			},
		},
	)
	worker.noWaitBetweenPingsForTesting = true

	idleMonitor := NewIdleMonitor(1)

	if err := worker.Start(ctx, idleMonitor); err != nil {
		t.Errorf("worker.Start() = %v", err)
	}

	if got, want := agent.Pings, 2; got != want {
		t.Errorf("agent.Pings = %d, want %d", got, want)
	}
	if got, want := job.State, JobStateFinished; got != want {
		t.Errorf("job.State = %q, want %q", got, want)
	}
}

func TestAgentWorker_DisconnectAfterJob_Start_Pause_Unpause(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := NewFakeAPIServer()
	defer server.Close()

	job := server.AddJob(map[string]string{
		"BUILDKITE_COMMAND": "echo echo",
	})

	// Pre-register the agent.
	const agentSessionToken = "alpacas"
	agent := server.AddAgent(agentSessionToken)
	agent.PingHandler = func() (api.Ping, error) {
		switch agent.Pings {
		case 0:
			return api.Ping{
				Job: job.Job,
			}, nil

		case 1:
			return api.Ping{
				Action:  "pause",
				Message: "Agent is now paused",
			}, nil

		case 2:
			return api.Ping{}, nil // now idle

		default:
			return api.Ping{}, errors.New("too many pings")
		}
	}

	server.Assign(agent, job)

	apiClient := api.NewClient(logger.Discard, api.Config{
		Endpoint: server.URL,
		Token:    "llamas",
	})

	l := logger.NewConsoleLogger(logger.NewTestPrinter(t), func(int) {})

	worker := NewAgentWorker(
		l,
		&api.AgentRegisterResponse{
			UUID:              uuid.New().String(),
			Name:              "agent-1",
			AccessToken:       "alpacas",
			Endpoint:          server.URL,
			PingInterval:      1,
			JobStatusInterval: 1,
			HeartbeatInterval: 10,
		},
		metrics.NewCollector(logger.Discard, metrics.CollectorConfig{}),
		apiClient,
		AgentWorkerConfig{
			SpawnIndex: 1,
			AgentConfiguration: AgentConfiguration{
				BootstrapScript:    "./dummy_bootstrap.sh",
				BuildPath:          filepath.Join(os.TempDir(), t.Name(), "build"),
				HooksPath:          filepath.Join(os.TempDir(), t.Name(), "hooks"),
				DisconnectAfterJob: true,
			},
		},
	)
	worker.noWaitBetweenPingsForTesting = true

	idleMonitor := NewIdleMonitor(1)

	if err := worker.Start(ctx, idleMonitor); err != nil {
		t.Errorf("worker.Start() = %v", err)
	}

	if got, want := agent.Pings, 3; got != want {
		t.Errorf("agent.Pings = %d, want %d", got, want)
	}
	if got, want := job.State, JobStateFinished; got != want {
		t.Errorf("job.State = %q, want %q", got, want)
	}
}
