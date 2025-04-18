// Copyright 2019 The Berglas Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package berglas

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/berglas/v2/pkg/berglas/logging"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type revokeRequest interface {
	isRevokeRequest()
}

// StorageRevokeRequest is used as input to revoke access to a from Cloud
// Storage encrypted with Cloud KMS.
type StorageRevokeRequest struct {
	// Bucket is the name of the bucket where the secret lives.
	Bucket string

	// Object is the name of the object in Cloud Storage.
	Object string

	// Members is the list of membership bindings. This should be in the format
	// described at https://godoc.org/google.golang.org/api/iam/v1#Binding.
	Members []string
}

func (r *StorageRevokeRequest) isRevokeRequest() {}

// RevokeRequest is an alias for StorageRevokeRequest for
// backwards-compatibility. New clients should use StorageRevokeRequest.
type RevokeRequest = StorageRevokeRequest

// SecretManagerRevokeRequest is used as input to revoke access to a secret in
// Secret Manager.
type SecretManagerRevokeRequest struct {
	// Project is the ID or number of the project where secrets live.
	Project string

	// Name is the name of the secret to access.
	Name string

	// Members is the list of membership bindings. This should be in the format
	// described at https://godoc.org/google.golang.org/api/iam/v1#Binding.
	Members []string
}

func (r *SecretManagerRevokeRequest) isRevokeRequest() {}

// Revoke is a top-level package function for revokeing access to a secret. For
// large volumes of secrets, please create a client instead.
func Revoke(ctx context.Context, i revokeRequest) error {
	client, err := New(ctx)
	if err != nil {
		return err
	}
	return client.Revoke(ctx, i)
}

// Revoke removes IAM permission to the given entity on the storage object and
// the underlying KMS key.
func (c *Client) Revoke(ctx context.Context, i revokeRequest) error {
	if i == nil {
		return fmt.Errorf("missing request")
	}

	switch t := i.(type) {
	case *SecretManagerRevokeRequest:
		return c.secretManagerRevoke(ctx, t)
	case *StorageRevokeRequest:
		return c.storageRevoke(ctx, t)
	default:
		return fmt.Errorf("unknown revoke type %T", t)
	}
}

func (c *Client) secretManagerRevoke(ctx context.Context, i *SecretManagerRevokeRequest) error {
	project := i.Project
	if project == "" {
		return fmt.Errorf("missing project")
	}

	name := i.Name
	if name == "" {
		return fmt.Errorf("missing secret name")
	}

	members := i.Members
	if len(members) == 0 {
		return nil
	}
	sort.Strings(members)

	logger := logging.FromContext(ctx).With(
		"project", project,
		"name", name,
		"members", members,
	)

	logger.DebugContext(ctx, "revoke.start")
	defer logger.DebugContext(ctx, "revoke.finish")

	logger.DebugContext(ctx, "revoking access to seetcr")

	storageHandle := c.secretManagerIAM(project, name)
	if err := updateIAMPolicy(ctx, storageHandle, func(p *iam.Policy) *iam.Policy {
		for _, m := range members {
			p.Remove(m, iamSecretManagerAccessor)
		}
		return p
	}); err != nil {
		terr, ok := grpcstatus.FromError(err)
		if ok && terr.Code() == grpccodes.NotFound {
			return errSecretDoesNotExist
		}

		return fmt.Errorf("failed to update Storage IAM policy for %s: %w", name, err)
	}

	return nil
}

func (c *Client) storageRevoke(ctx context.Context, i *StorageRevokeRequest) error {
	bucket := i.Bucket
	if bucket == "" {
		return fmt.Errorf("missing bucket name")
	}

	object := i.Object
	if object == "" {
		return fmt.Errorf("missing object name")
	}

	members := i.Members
	if len(members) == 0 {
		return nil
	}
	sort.Strings(members)

	logger := logging.FromContext(ctx).With(
		"bucket", bucket,
		"object", object,
		"members", members,
	)

	logger.DebugContext(ctx, "revoke.start")
	defer logger.DebugContext(ctx, "revoke.finish")

	// Get attributes to find the KMS key
	logger.DebugContext(ctx, "finding storage object")

	objHandle := c.storageClient.Bucket(bucket).Object(object)
	attrs, err := objHandle.Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return errSecretDoesNotExist
	}
	if err != nil {
		return fmt.Errorf("failed to read secret metadata: %w", err)
	}
	if attrs.Metadata == nil || attrs.Metadata[MetadataKMSKey] == "" {
		return fmt.Errorf("missing kms key in secret metadata")
	}
	key := attrs.Metadata[MetadataKMSKey]

	logger = logger.With("key", key)
	logger.DebugContext(ctx, "found kms key")

	// Remove access to storage
	logger.DebugContext(ctx, "revoking access to storage")

	storageHandle := c.storageIAM(bucket, object)
	if err := updateIAMPolicy(ctx, storageHandle, func(p *iam.Policy) *iam.Policy {
		for _, m := range members {
			p.Remove(m, iamObjectReader)
		}
		return p
	}); err != nil {
		return fmt.Errorf("failed to update Storage IAM policy for %s: %w", object, err)
	}

	// Remove access to KMS
	logger.DebugContext(ctx, "revoking access to kms")

	kmsHandle := c.kmsClient.ResourceIAM(key)
	if err := updateIAMPolicy(ctx, kmsHandle, func(p *iam.Policy) *iam.Policy {
		for _, m := range members {
			p.Remove(m, iamKMSDecrypt)
		}
		return p
	}); err != nil {
		return fmt.Errorf("failed to update KMS IAM policy for %s: %w", key, err)
	}

	return nil
}
