// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/glasskube/distr/internal/apierrors"
	internalctx "github.com/glasskube/distr/internal/context"
	"github.com/glasskube/distr/internal/db"
	"github.com/glasskube/distr/internal/registry/audit"
	"github.com/glasskube/distr/internal/registry/authz"
	"github.com/glasskube/distr/internal/registry/blob"
	registryerror "github.com/glasskube/distr/internal/registry/error"
	"github.com/glasskube/distr/internal/registry/manifest"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

type catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type manifests struct {
	blobHandler     blob.BlobHandler
	manifestHandler manifest.ManifestHandler
	authz           authz.Authorizer
	audit           audit.ArtifactAuditor
	log             *zap.SugaredLogger
}

func isManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

func isTags(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "tags"
}

func isCatalog(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 2 {
		return false
	}

	return elems[len(elems)-1] == "_catalog"
}

// Returns whether this url should be handled by the referrers handler
func isReferrers(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "referrers"
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (handler *manifests) handle(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	switch req.Method {
	case http.MethodGet:
		if err := handler.authz.AuthorizeReference(req.Context(), repo, target, authz.ActionRead); err != nil {
			if errors.Is(err, authz.ErrAccessDenied) {
				return regErrDenied
			} else if errors.Is(err, registryerror.ErrInvalidArtifactName) {
				return regErrNameInvalid
			}
			return regErrInternal(err)
		}
		return handler.handleGet(resp, req, repo, target)
	case http.MethodHead:
		if err := handler.authz.AuthorizeReference(req.Context(), repo, target, authz.ActionStat); err != nil {
			if errors.Is(err, authz.ErrAccessDenied) {
				return regErrDenied
			} else if errors.Is(err, registryerror.ErrInvalidArtifactName) {
				return regErrNameInvalid
			}
			return regErrInternal(err)
		}
		return handler.handleHead(resp, req, repo, target)
	case http.MethodPut:
		if err := handler.authz.AuthorizeReference(req.Context(), repo, target, authz.ActionWrite); err != nil {
			if errors.Is(err, authz.ErrAccessDenied) {
				return regErrDenied
			} else if errors.Is(err, registryerror.ErrInvalidArtifactName) {
				return regErrNameInvalid
			}
			return regErrInternal(err)
		}
		return handler.handlePut(resp, req, repo, target)
	// case http.MethodDelete:
	// 	if err := handler.authz.AuthorizeReference(req.Context(), repo, target, authz.ActionWrite); err != nil {
	// 		if errors.Is(err, authz.ErrAccessDenied) {
	// 			return regErrDenied
	// 		}
	// 		return regErrInternal(err)
	// 	}
	// 	return handler.handleDelete(resp, req, repo, target)
	default:
		return regErrMethodUnknown
	}
}

func (m *manifests) handleTags(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	if req.Method == http.MethodGet {
		if err := m.authz.Authorize(req.Context(), repo, authz.ActionRead); err != nil {
			if errors.Is(err, authz.ErrAccessDenied) {
				return regErrDenied
			} else if errors.Is(err, registryerror.ErrInvalidArtifactName) {
				return regErrNameInvalid
			}
			return regErrInternal(err)
		}

		last := req.URL.Query().Get("last")
		n := 10000
		if ns := req.URL.Query().Get("n"); ns != "" {
			if parsed, err := strconv.Atoi(ns); err != nil {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "BAD_REQUEST",
					Message: fmt.Sprintf("parsing n: %v", err),
				}
			} else {
				n = parsed
			}
		}

		references, err := m.manifestHandler.ListTags(req.Context(), repo, n, last)
		if errors.Is(err, manifest.ErrNameUnknown) {
			return regErrNameUnknown
		} else if err != nil {
			return regErrInternal(err)
		}

		tagsToList := listTags{
			Name: repo,
			Tags: references,
		}

		msg, err := json.Marshal(tagsToList)
		if err != nil {
			return regErrInternal(err)
		}
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		if _, err := io.Copy(resp, bytes.NewReader(msg)); err != nil {
			return regErrInternal(err)
		}
		return nil
	}

	return regErrMethodUnknown
}

func (m *manifests) handleCatalog(resp http.ResponseWriter, req *http.Request) *regError {
	query := req.URL.Query()
	nStr := query.Get("n")
	n := 10000
	if nStr != "" {
		n, _ = strconv.Atoi(nStr)
	}

	if req.Method == http.MethodGet {
		repos, err := m.manifestHandler.List(req.Context(), n)
		if err != nil {
			return regErrInternal(err)
		}

		repositoriesToList := catalog{Repos: repos}

		msg, err := json.Marshal(repositoriesToList)
		if err != nil {
			return regErrInternal(err)
		}
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		if _, err := io.Copy(resp, bytes.NewReader(msg)); err != nil {
			return regErrInternal(err)
		}
		return nil
	}

	return regErrMethodUnknown
}

// TODO: implement handling of artifactType querystring
func (m *manifests) handleReferrers(resp http.ResponseWriter, req *http.Request) *regError {
	// Ensure this is a GET request
	if req.Method != http.MethodGet {
		return regErrMethodUnknown
	}

	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	if err := m.authz.AuthorizeReference(req.Context(), repo, target, authz.ActionRead); err != nil {
		if errors.Is(err, authz.ErrAccessDenied) {
			return regErrDenied
		} else if errors.Is(err, registryerror.ErrInvalidArtifactName) {
			return regErrNameInvalid
		}
		return regErrInternal(err)
	}

	// Validate that incoming target is a valid digest
	if _, err := v1.NewHash(target); err != nil {
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "UNSUPPORTED",
			Message: "Target must be a valid digest",
		}
	}

	digests, err := m.manifestHandler.ListDigests(req.Context(), repo)
	if errors.Is(err, manifest.ErrNameUnknown) {
		return regErrNameUnknown
	} else if err != nil {
		return regErrInternal(err)
	}

	im := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     []v1.Descriptor{},
	}
	for _, reference := range digests {
		manifest, err := m.manifestHandler.Get(req.Context(), repo, reference.String())
		if err != nil {
			return regErrInternal(err)
		}

		b, err := m.blobHandler.Get(req.Context(), repo, manifest.Blob.Digest, false)
		if err != nil {
			return &regError{
				Status:  http.StatusNotFound,
				Code:    "BAD_REQUEST",
				Message: err.Error(),
			}
		}
		defer b.Close()
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, b); err != nil {
			return regErrInternal(err)
		}

		var refPointer struct {
			Subject *v1.Descriptor `json:"subject"`
		}
		_ = json.Unmarshal(buf.Bytes(), &refPointer)
		if refPointer.Subject == nil {
			continue
		}
		referenceDigest := refPointer.Subject.Digest
		if referenceDigest.String() != target {
			continue
		}
		// At this point, we know the current digest references the target
		var imageAsArtifact struct {
			Config struct {
				MediaType string `json:"mediaType"`
			} `json:"config"`
		}
		_ = json.Unmarshal(buf.Bytes(), &imageAsArtifact)
		im.Manifests = append(im.Manifests, v1.Descriptor{
			MediaType:    types.MediaType(manifest.ContentType),
			Size:         int64(buf.Len()),
			Digest:       reference,
			ArtifactType: imageAsArtifact.Config.MediaType,
		})
	}
	msg, err := json.Marshal(&im)
	if err != nil {
		return regErrInternal(err)
	}
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.Header().Set("Content-Type", string(types.OCIImageIndex))
	resp.WriteHeader(http.StatusOK)
	if _, err := io.Copy(resp, bytes.NewReader(msg)); err != nil {
		return regErrInternal(err)
	}
	return nil
}

func (handler *manifests) handleGet(resp http.ResponseWriter, req *http.Request, repo, target string) *regError {
	ctx := req.Context()
	m, err := handler.manifestHandler.Get(ctx, repo, target)
	if errors.Is(err, manifest.ErrNameUnknown) {
		return regErrNameUnknown
	} else if errors.Is(err, manifest.ErrManifestUnknown) {
		return regErrManifestUnknown
	} else if err != nil {
		return regErrInternal(err)
	}

	b, err := handler.blobHandler.Get(ctx, repo, m.Blob.Digest, true)
	if err != nil {
		var rerr blob.RedirectError
		if errors.As(err, &rerr) {
			if err := handler.audit.AuditPull(ctx, repo, target); err != nil {
				log := internalctx.GetLogger(ctx)
				log.Warn("failed to audit-log pull", zap.Error(err))
				sentry.GetHubFromContext(ctx)
			}
			http.Redirect(resp, req, rerr.Location, rerr.Code)
			return nil
		}
		// TODO: More nuanced
		return regErrManifestUnknown
	}
	defer b.Close()

	buf := bytes.Buffer{}
	if _, err = io.Copy(&buf, b); err != nil {
		return regErrInternal(err)
	}

	resp.Header().Set("Docker-Content-Digest", m.Blob.Digest.String())
	resp.Header().Set("Content-Type", m.ContentType)
	resp.Header().Set("Content-Length", fmt.Sprint(buf.Len()))
	resp.WriteHeader(http.StatusOK)
	if _, err := io.Copy(resp, &buf); err != nil {
		return regErrInternal(err)
	}
	if err := handler.audit.AuditPull(ctx, repo, target); err != nil {
		log := internalctx.GetLogger(ctx)
		log.Warn("failed to audit-log pull", zap.Error(err))
		sentry.GetHubFromContext(ctx)
	}
	return nil
}

func (handler *manifests) handleHead(resp http.ResponseWriter, req *http.Request, repo, target string) *regError {
	ctx := req.Context()
	m, err := handler.manifestHandler.Get(ctx, repo, target)
	if errors.Is(err, manifest.ErrNameUnknown) {
		return regErrNameUnknown
	} else if errors.Is(err, manifest.ErrManifestUnknown) {
		return regErrManifestUnknown
	} else if err != nil {
		return regErrInternal(err)
	}

	bsh, ok := handler.blobHandler.(blob.BlobStatHandler)
	if !ok {
		return regErrInternal(errors.New("cannot stat blob"))
	}

	l, err := bsh.Stat(ctx, repo, m.Blob.Digest)
	if err != nil {
		// TODO: More nuanced
		return regErrManifestUnknown
	}

	if err := handler.audit.AuditPull(ctx, repo, target); err != nil {
		log := internalctx.GetLogger(ctx)
		log.Warn("failed to audit-log pull", zap.Error(err))
		sentry.GetHubFromContext(ctx)
	}

	resp.Header().Set("Docker-Content-Digest", m.Blob.Digest.String())
	resp.Header().Set("Content-Type", m.ContentType)
	resp.Header().Set("Content-Length", fmt.Sprint(l))
	resp.WriteHeader(http.StatusOK)
	return nil
}

func (handler *manifests) handlePut(resp http.ResponseWriter, req *http.Request, repo, target string) *regError {
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, req.Body); err != nil {
		return regErrInternal(err)
	}

	mf := manifest.Manifest{
		ContentType: req.Header.Get("Content-Type"),
		Blob: manifest.Blob{
			Size: int64(buf.Len()),
		},
	}
	if manifestDigest, _, err := v1.SHA256(bytes.NewReader(buf.Bytes())); err != nil {
		return regErrInternal(err)
	} else {
		mf.Blob.Digest = manifestDigest
	}

	var blobs []manifest.Blob

	// If the manifest is a manifest list, check that the manifest
	// list's constituent manifests are already uploaded.
	// This isn't strictly required by the registry API, but some
	// registries require this.
	if types.MediaType(mf.ContentType).IsIndex() {
		if err := func(ctx context.Context) *regError {
			im, err := v1.ParseIndexManifest(bytes.NewReader(buf.Bytes()))
			if err != nil {
				return regErrManifestInvalid(err)
			}
			for _, desc := range im.Manifests {
				if !desc.MediaType.IsDistributable() {
					continue
				}
				if desc.MediaType.IsIndex() || desc.MediaType.IsImage() {
					if _, err := handler.manifestHandler.Get(ctx, repo, desc.Digest.String()); err != nil {
						return &regError{
							Status:  http.StatusNotFound,
							Code:    "MANIFEST_UNKNOWN",
							Message: fmt.Sprintf("Sub-manifest %q not found", desc.Digest),
						}
					}
					blobs = append(blobs, manifest.Blob{Digest: desc.Digest, Size: desc.Size})
				} else {
					// TODO: Probably want to do an existence check for blobs.
					handler.log.Warnf("TODO: Check blobs for %q (MediaType: %v)", desc.Digest, desc.MediaType)
				}
			}
			return nil
		}(req.Context()); err != nil {
			return err
		}
	} else if types.MediaType(mf.ContentType).IsImage() {
		if err := func() *regError {
			m, err := v1.ParseManifest(bytes.NewReader(buf.Bytes()))
			if err != nil {
				return regErrManifestInvalid(err)
			}
			blobs = append(blobs, manifest.Blob{Digest: m.Config.Digest, Size: m.Config.Size})
			if m.Subject != nil {
				blobs = append(blobs, manifest.Blob{Digest: m.Subject.Digest, Size: m.Subject.Size})
			}
			for _, desc := range m.Layers {
				if !desc.MediaType.IsDistributable() {
					continue
				}
				// TODO: Maybe check if the layer was already uploaded
				blobs = append(blobs, manifest.Blob{Digest: desc.Digest, Size: desc.Size})
			}
			return nil
		}(); err != nil {
			return err
		}
	}

	if err := checkIncompatibleManifest(buf.Bytes()); err != nil {
		return err
	}

	if bph, ok := handler.blobHandler.(blob.BlobPutHandler); !ok {
		return regErrInternal(errors.New("blob handler is not a BlobPutHandler"))
	} else {
		if err := bph.Put(req.Context(), repo, mf.Blob.Digest, mf.ContentType, buf); err != nil {
			return regErrInternal(err)
		}
	}

	// Allow future references by target (tag) and immutable digest.
	// See https://docs.docker.com/engine/reference/commandline/pull/#pull-an-image-by-digest-immutable-identifier.
	err := db.RunTx(req.Context(), func(ctx context.Context) error {
		return multierr.Combine(
			handler.manifestHandler.Put(ctx, repo, mf.Blob.Digest.String(), mf, blobs),
			handler.manifestHandler.Put(ctx, repo, target, mf, blobs),
		)
	})
	if errors.Is(err, apierrors.ErrQuotaExceeded) {
		return regErrDeniedQuotaExceeded
	} else if err != nil {
		return regErrInternal(err)
	}

	resp.Header().Set("Docker-Content-Digest", mf.Blob.Digest.String())
	resp.Header().Set("OCI-Subject", mf.Blob.Digest.String())
	resp.Header().Set("Location", req.URL.JoinPath(mf.Blob.Digest.String()).Path)
	resp.WriteHeader(http.StatusCreated)
	return nil
}

// func (handler *manifests) handleDelete(resp http.ResponseWriter, req *http.Request, repo, target string) *regError {
// 	if err := handler.manifestHandler.Delete(req.Context(), repo, target); errors.Is(err, manifest.ErrNameUnknown) {
// 		return regErrNameUnknown
// 	} else if errors.Is(err, manifest.ErrManifestUnknown) {
// 		return regErrManifestUnknown
// 	} else if err != nil {
// 		regErrInternal(err)
// 	}

// 	resp.WriteHeader(http.StatusAccepted)
// 	return nil
// }

func checkIncompatibleManifest(data []byte) *regError {
	var mf struct {
		Blobs []any `json:"blobs"`
	}
	if err := json.Unmarshal(data, &mf); err != nil {
		return regErrManifestInvalid(err)
	} else if len(mf.Blobs) > 0 {
		return regErrManifestInvalid(errors.New("non-compliant manifest with blobs entry detected"))
	}
	return nil
}
