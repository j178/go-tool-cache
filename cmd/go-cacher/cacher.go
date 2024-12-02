// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The go-cacher binary is a cacher helper program that cmd/go can use.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	neturl "net/url"
	"os"
	"path/filepath"

	smithyendpoints "github.com/aws/smithy-go/endpoints"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/bradfitz/go-tool-cache/cacheproc"
	"github.com/bradfitz/go-tool-cache/cachers"
)

const defaultCacheKey = "v1"

// All the following env variable names are optional
const (
	// path to local disk directory. defaults to os.UserCacheDir()/go-cacher
	envVarDiskCacheDir = "GOCACHE_DISK_DIR"

	// S3 cache
	envVarS3Endpoint           = "GOCACHE_S3_ENDPOINT"
	envVarS3CacheRegion        = "GOCACHE_AWS_REGION"
	envVarS3AwsAccessKey       = "GOCACHE_AWS_ACCESS_KEY"
	envVarS3AwsSecretAccessKey = "GOCACHE_AWS_SECRET_KEY"
	envVarS3AwsCredsProfile    = "GOCACHE_AWS_CREDS_PROFILE"
	envVarS3BucketName         = "GOCACHE_S3_BUCKET"
	envVarS3CacheKey           = "GOCACHE_CACHE_KEY"

	// HTTP cache - optional cache server HTTP prefix (scheme and authority only);
	envVarHttpCacheServerBase = "GOCACHE_HTTP_SERVER_BASE"
)

var (
	verbose = flag.Bool("verbose", false, "be verbose")
)

type Env interface {
	Get(key string) string
}

type osEnv struct{}

func (osEnv) Get(key string) string {
	return os.Getenv(key)
}

func getAwsConfigFromEnv(ctx context.Context, env Env) (*aws.Config, error) {
	// read from env
	awsRegion := env.Get(envVarS3CacheRegion)
	if awsRegion == "" {
		return nil, nil
	}
	accessKey := env.Get(envVarS3AwsAccessKey)
	secretAccessKey := env.Get(envVarS3AwsSecretAccessKey)
	if accessKey != "" && secretAccessKey != "" {
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion(awsRegion),
			config.WithCredentialsProvider(credentials.StaticCredentialsProvider{
				Value: aws.Credentials{
					AccessKeyID:     accessKey,
					SecretAccessKey: secretAccessKey,
				},
			}))
		if err != nil {
			return nil, err
		}
		endpoint := env.Get(envVarS3Endpoint)
		if endpoint != "" {
			cfg.BaseEndpoint = &endpoint
		}
		return &cfg, nil
	}
	credsProfile := env.Get(envVarS3AwsCredsProfile)
	if credsProfile != "" {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(awsRegion), config.WithSharedConfigProfile(credsProfile))
		if err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	return nil, nil
}

type resolver struct {
	endpoint string
}

func (*resolver) ResolveEndpoint(_ context.Context, params s3.EndpointParameters) (
	smithyendpoints.Endpoint, error,
) {
	url := fmt.Sprintf("%s/%s", *params.Endpoint, *params.Bucket)
	u, err := neturl.Parse(url)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	if params.Region != nil {
		q := u.Query()
		q.Set("region", *params.Region)
		u.RawQuery = q.Encode()
	}

	return smithyendpoints.Endpoint{
		URI: *u,
	}, nil
}

func maybeS3Cache(ctx context.Context, env Env) (cachers.RemoteCache, error) {
	awsConfig, err := getAwsConfigFromEnv(ctx, env)
	if err != nil {
		return nil, err
	}
	bucket := env.Get(envVarS3BucketName)
	if bucket == "" || awsConfig == nil {
		// We need at least name of bucket and valid aws config
		return nil, nil
	}
	cacheKey := env.Get(envVarS3CacheKey)
	if cacheKey == "" {
		cacheKey = defaultCacheKey
	}

	s3Client := s3.NewFromConfig(*awsConfig, s3.WithEndpointResolverV2(&resolver{}))
	s3Cache := cachers.NewS3Cache(s3Client, bucket, cacheKey, *verbose)
	return s3Cache, nil
}

func getCache(ctx context.Context, env Env, verbose bool) cachers.LocalCache {
	dir := getDir(env)
	var local cachers.LocalCache = cachers.NewSimpleDiskCache(verbose, dir)

	remote, err := maybeS3Cache(ctx, env)
	if err != nil {
		log.Fatal(err)
	}
	if remote == nil {
		remote, err = maybeHttpCache(env)
		if err != nil {
			log.Fatal(err)
		}
	}

	if remote != nil {
		return cachers.NewCombinedCache(local, remote, verbose)
	}
	if verbose {
		return cachers.NewLocalCacheStates(local)
	}
	return local
}

func maybeHttpCache(env Env) (cachers.RemoteCache, error) {
	serverBase := env.Get(envVarHttpCacheServerBase)
	if serverBase == "" {
		return nil, nil
	}
	return cachers.NewHttpCache(serverBase, *verbose), nil
}

func getDir(env Env) string {
	dir := env.Get(envVarDiskCacheDir)
	if dir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			log.Fatal(err)
		}
		d = filepath.Join(d, "go-cacher")
		dir = d
	}
	return dir
}

func main() {
	flag.Parse()
	env := &osEnv{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cache := getCache(ctx, env, *verbose)
	proc := cacheproc.NewCacheProc(cache)
	if err := proc.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
