package secrets

import (
	"context"
	"fmt"
	"sync"

	"github.com/buildkite/agent/v3/api"
	"golang.org/x/sync/semaphore"
)

// APIClient interface defines only the method needed by the secrets manager
// to fetch secrets from the Buildkite API.
type APIClient interface {
	GetSecret(ctx context.Context, req *api.GetSecretRequest) (*api.Secret, *api.Response, error)
}

// Secret represents a fetched secret with its key and value.
type Secret struct {
	Key   string
	Value string
}

type SecretError struct {
	Key string
	Err error
}

func (e *SecretError) Error() string {
	return fmt.Sprintf("secret %q: %s", e.Key, e.Err.Error())
}

func (e *SecretError) Unwrap() error {
	return e.Err
}

// FetchSecrets retrieves all secret values from the API sequentially.
// If any secret fails, returns error with details of all failed secrets.
func FetchSecrets(ctx context.Context, client APIClient, jobID string, keys []string, concurrency int) ([]Secret, []error) {
	secrets := make([]Secret, 0, len(keys))
	secretsMu := sync.Mutex{}

	errs := make([]error, 0, len(keys))
	errsMu := sync.Mutex{}

	sem := semaphore.NewWeighted(int64(concurrency))

	for _, key := range keys {
		if err := sem.Acquire(ctx, 1); err != nil {
			errsMu.Lock()
			errs = append(errs, fmt.Errorf("failed to acquire semaphore for key %q: %w", key, err))
			errsMu.Unlock()
			break
		}

		go func(key string) {
			defer sem.Release(1)
			apiSecret, _, err := client.GetSecret(ctx, &api.GetSecretRequest{Key: key, JobID: jobID})
			if err != nil {
				errsMu.Lock()
				errs = append(errs, &SecretError{
					Key: key,
					Err: err,
				})
				errsMu.Unlock()
				return
			}

			secretsMu.Lock()
			defer secretsMu.Unlock()
			secrets = append(secrets, Secret{
				Key:   key,
				Value: apiSecret.Value,
			})
		}(key)
	}

	err := sem.Acquire(ctx, int64(concurrency)) // Wait for all goroutines to finish
	if err != nil {
		return nil, []error{fmt.Errorf("failed to acquire semaphore waiting for jobs to finish: %w", err)}
	}

	// If any secret fails, return error with details of all failed secrets
	if len(errs) > 0 {
		return nil, errs
	}

	return secrets, nil
}
