/*
Copyright 2020 The Kubernetes Authors.

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

package host

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"
)

// TODO: investigate if we can get rid of this or simplify it.
// The original requirement was due to CI images for kubernetes being different that the production ones:
// CI images are single arch, with -amd64 suffix, while production images are multi-arch.

// GetArchiveTags obtains a list of "repo:tag" docker image tags from a
// given docker image archive (tarball) path
// compatible with all known specs:
// https://github.com/moby/moby/blob/master/image/spec/v1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.2.md
func GetArchiveTags(path string) ([]string, error) {
	// open the archive and find the repositories entry
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var hdr *tar.Header
	for {
		hdr, err = tr.Next()
		if err == io.EOF {
			return nil, errors.New("could not find image metadata")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "repositories" {
			break
		}
	}
	// read and parse the tags
	b, err := io.ReadAll(tr)
	if err != nil {
		return nil, err
	}
	// parse
	repoTags, err := parseRepositories(b)
	if err != nil {
		return nil, err
	}
	// convert to tags in the docker CLI sense
	res := []string{}
	for repo, tags := range repoTags {
		for tag := range tags {
			res = append(res, fmt.Sprintf("%s:%s", repo, tag))
		}
	}
	return res, nil
}

// Temporary workaround to allow detecting the transition to registry.k8s.io in kubeadm 1.22~1.25.
// TODO: remove KubeadmVer and replaceKubeadm125Repository() once we no longer test the
// kubeadm 1.25 / k8s 1.24 skew

// KubeadmBinaryVer tracks the version of the kubeadm binary
var KubeadmBinaryVer string

func replaceKubeadm125Repository(repository string) string {
	if len(KubeadmBinaryVer) == 0 {
		return repository
	}
	v := version.MustParseSemantic(KubeadmBinaryVer)
	if v.AtLeast(version.MustParseSemantic("v1.22.0-0")) && strings.HasPrefix(repository, "k8s.gcr.io") {
		return strings.Replace(repository, "k8s.gcr.io", "registry.k8s.io", -1)
	}
	return repository
}

// EditArchiveRepositories applies edit to reader's image repositories,
// IE the repository part of repository:tag in image tags
// This supports v1 / v1.1 / v1.2 Docker Image Archives
//
// editRepositories should be a function that returns the input or an edited
// form, where the input is the image repository
//
// https://github.com/moby/moby/blob/master/image/spec/v1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.2.md
func EditArchiveRepositories(reader io.Reader, writer io.Writer, editRepositories func(string) string) error {
	tarReader := tar.NewReader(reader)
	tarWriter := tar.NewWriter(writer)
	// iterate all entries in the tarball
	for {
		// read an entry
		hdr, err := tarReader.Next()
		if err == io.EOF {
			return tarWriter.Close()
		} else if err != nil {
			return err
		}
		b, err := io.ReadAll(tarReader)
		if err != nil {
			return err
		}

		// edit the repostories and manifests files when we find them
		if hdr.Name == "repositories" {
			b, err = editRepositoriesFile(b, editRepositories)
			if err != nil {
				return err
			}
			hdr.Size = int64(len(b))
		} else if hdr.Name == "manifest.json" {
			b, err = editManifestRepositories(b, editRepositories)
			if err != nil {
				return err
			}
			hdr.Size = int64(len(b))
		}

		// write to the output tarball
		if err := tarWriter.WriteHeader(hdr); err != nil {
			return err
		}
		if len(b) > 0 {
			if _, err := tarWriter.Write(b); err != nil {
				return err
			}
		}
	}
}

/* helpers */

// archiveRepositories represents repository:tag:ref
//
// https://github.com/moby/moby/blob/master/image/spec/v1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.1.md
// https://github.com/moby/moby/blob/master/image/spec/v1.2.md
type archiveRepositories map[string]map[string]string

func editRepositoriesFile(raw []byte, editRepositories func(string) string) ([]byte, error) {
	tags, err := parseRepositories(raw)
	if err != nil {
		return nil, err
	}

	fixed := make(archiveRepositories)
	for repository, tagsToRefs := range tags {
		repository = replaceKubeadm125Repository(repository)
		fixed[editRepositories(repository)] = tagsToRefs
	}

	return json.Marshal(fixed)
}

// https://github.com/moby/moby/blob/master/image/spec/v1.2.md#combined-image-json--filesystem-changeset-format
type metadataEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// applies
func editManifestRepositories(raw []byte, editRepositories func(string) string) ([]byte, error) {
	var entries []metadataEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}

	for i, entry := range entries {
		fixed := make([]string, len(entry.RepoTags))
		for i, tag := range entry.RepoTags {
			parts := strings.Split(tag, ":")
			if len(parts) > 2 {
				return nil, fmt.Errorf("invalid repotag: %s", entry)
			}
			parts[0] = editRepositories(parts[0])
			parts[0] = replaceKubeadm125Repository(parts[0])
			fixed[i] = strings.Join(parts, ":")
		}

		entries[i].RepoTags = fixed
	}

	return json.Marshal(entries)
}

// returns repository:tag:ref
func parseRepositories(data []byte) (archiveRepositories, error) {
	var repoTags archiveRepositories
	if err := json.Unmarshal(data, &repoTags); err != nil {
		return nil, err
	}
	return repoTags, nil
}
