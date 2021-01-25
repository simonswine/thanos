// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package azure

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/Azure/azure-pipeline-go/pipeline"
	blob "github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
)

// DirDelim is the delimiter used to model a directory structure in an object store bucket.
const DirDelim = "/"

var errorCodeRegex = regexp.MustCompile(`X-Ms-Error-Code:\D*\[(\w+)\]`)

func init() {
	// Disable `ForceLog` in Azure storage module
	// As the time of this patch, the logging function in the storage module isn't correctly
	// detecting expected REST errors like 404 and so outputs them to syslog along with a stacktrace.
	// https://github.com/Azure/azure-storage-blob-go/issues/214
	//
	// This needs to be done at startup because the underlying variable is not thread safe.
	// https://github.com/Azure/azure-pipeline-go/blob/dc95902f1d32034f8f743ccc6c3f2eb36b84da27/pipeline/core.go#L276-L283
	pipeline.SetForceLogEnabled(false)
}

func getContainerURL(ctx context.Context, logger log.Logger, conf Config) (blob.ContainerURL, error) {
	var c blob.Credential

	if conf.StorageAccountKey == "" {
		msiEndpoint, err := adal.GetMSIEndpoint()
		if err != nil {
			return blob.ContainerURL{}, errors.Wrap(err, "unable to get msi endpoint")

		}

		sp, err := adal.NewServicePrincipalTokenFromMSI(msiEndpoint, "https://storage.azure.com/")
		if err != nil {
			return blob.ContainerURL{}, errors.Wrap(err, "unable to get service principal token")
		}

		// initial refresh
		if err := sp.Refresh(); err != nil {
			return blob.ContainerURL{}, errors.Wrap(err, "unable to refresh service principal token")
		}

		c = blob.NewTokenCredential(sp.Token().AccessToken, func(cred blob.TokenCredential) time.Duration {
			// TODO: verify implementation
			// TODO: backoff on errors

			level.Debug(logger).Log("msg", "refresh Azure token")

			if err := sp.Refresh(); err != nil {
				level.Error(logger).Log("msg", "error refreshing Azure token", "err", err)
				return time.Second
			}

			token := sp.Token()
			cred.SetToken(token.AccessToken)

			// refresh 5 minutes before expiry
			refreshDuration := token.Expires().UTC().Sub(time.Now())
			level.Debug(logger).Log("msg", "token refresh", "expires_in", refreshDuration)

			refreshDuration -= 5 * time.Minute

			if refreshDuration < 0 {
				return time.Second
			}

			return refreshDuration
		})

	} else {
		var err error
		c, err = blob.NewSharedKeyCredential(conf.StorageAccountName, conf.StorageAccountKey)
		if err != nil {
			return blob.ContainerURL{}, err
		}
	}

	retryOptions := blob.RetryOptions{
		MaxTries: int32(conf.MaxRetries),
	}
	if deadline, ok := ctx.Deadline(); ok {
		retryOptions.TryTimeout = time.Until(deadline)
	}

	p := blob.NewPipeline(c, blob.PipelineOptions{
		Retry:     retryOptions,
		Telemetry: blob.TelemetryOptions{Value: "Thanos"},
		RequestLog: blob.RequestLogOptions{
			// Log a warning if an operation takes longer than the specified duration.
			// (-1=no logging; 0=default 3s threshold)
			LogWarningIfTryOverThreshold: -1,
		},
		Log: pipeline.LogOptions{
			ShouldLog: nil,
		},
	})
	u, err := url.Parse(fmt.Sprintf("https://%s.%s", conf.StorageAccountName, conf.Endpoint))
	if err != nil {
		return blob.ContainerURL{}, err
	}
	service := blob.NewServiceURL(*u, p)

	return service.NewContainerURL(conf.ContainerName), nil
}

func getContainer(ctx context.Context, logger log.Logger, conf Config) (blob.ContainerURL, error) {
	c, err := getContainerURL(ctx, logger, conf)
	if err != nil {
		return blob.ContainerURL{}, err
	}
	// Getting container properties to check if it exists or not. Returns error which will be parsed further.
	_, err = c.GetProperties(ctx, blob.LeaseAccessConditions{})
	return c, err
}

func createContainer(ctx context.Context, logger log.Logger, conf Config) (blob.ContainerURL, error) {
	c, err := getContainerURL(ctx, logger, conf)
	if err != nil {
		return blob.ContainerURL{}, err
	}
	_, err = c.Create(
		ctx,
		blob.Metadata{},
		blob.PublicAccessNone)
	return c, err
}

func getBlobURL(ctx context.Context, logger log.Logger, conf Config, blobName string) (blob.BlockBlobURL, error) {
	c, err := getContainerURL(ctx, logger, conf)
	if err != nil {
		return blob.BlockBlobURL{}, err
	}
	return c.NewBlockBlobURL(blobName), nil
}

func parseError(errorCode string) string {
	match := errorCodeRegex.FindStringSubmatch(errorCode)
	if len(match) == 2 {
		return match[1]
	}
	return errorCode
}
