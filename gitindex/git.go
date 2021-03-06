// Copyright 2016 Google Inc. All rights reserved.
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

package gitindex

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/zoekt"
	"github.com/google/zoekt/build"

	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/object"

	git "gopkg.in/src-d/go-git.v4"
	plumcfg "gopkg.in/src-d/go-git.v4/plumbing/format/config"
)

// RepoModTime returns the time of last fetch of a git repository.
func RepoModTime(dir string) (time.Time, error) {
	var last time.Time
	refDir := filepath.Join(dir, "refs")
	if _, err := os.Lstat(refDir); err == nil {
		if err := filepath.Walk(refDir,
			func(name string, fi os.FileInfo, err error) error {
				if !fi.IsDir() && last.Before(fi.ModTime()) {
					last = fi.ModTime()
				}
				return nil
			}); err != nil {
			return last, err
		}
	}

	// git gc compresses refs into the following file:
	for _, fn := range []string{"info/refs", "packed-refs"} {
		if fi, err := os.Lstat(filepath.Join(dir, fn)); err == nil && !fi.IsDir() && last.Before(fi.ModTime()) {
			last = fi.ModTime()
		}
	}

	return last, nil
}

// FindGitRepos finds directories holding git repositories.
func FindGitRepos(arg string) ([]string, error) {
	arg, err := filepath.Abs(arg)
	if err != nil {
		return nil, err
	}
	var dirs []string
	if err := filepath.Walk(arg, func(name string, fi os.FileInfo, err error) error {
		if fi, err := os.Lstat(filepath.Join(name, ".git")); err == nil && fi.IsDir() {
			dirs = append(dirs, filepath.Join(name, ".git"))
			return filepath.SkipDir
		}

		if !strings.HasSuffix(name, ".git") || !fi.IsDir() {
			return nil
		}

		fi, err = os.Lstat(filepath.Join(name, "objects"))
		if err != nil || !fi.IsDir() {
			return nil
		}

		dirs = append(dirs, name)
		return filepath.SkipDir
	}); err != nil {
		return nil, err
	}

	return dirs, nil
}

// setTemplates fills in URL templates for known git hosting
// sites.
func setTemplates(repo *zoekt.Repository, u *url.URL, typ string) error {
	repo.URL = u.String()
	switch typ {
	case "gitiles":
		/// eg. https://gerrit.googlesource.com/gitiles/+/master/tools/run_dev.sh#20
		repo.CommitURLTemplate = u.String() + "/+/{{.Version}}"
		repo.FileURLTemplate = u.String() + "/+/{{.Version}}/{{.Path}}"
		repo.LineFragmentTemplate = "{{.LineNumber}}"
	case "github":
		// eg. https://github.com/hanwen/go-fuse/blob/notify/genversion.sh#L10
		repo.CommitURLTemplate = u.String() + "/commit/{{.Version}}"
		repo.FileURLTemplate = u.String() + "/blob/{{.Version}}/{{.Path}}"
		repo.LineFragmentTemplate = "L{{.LineNumber}}"

	case "cgit":

		// http://git.savannah.gnu.org/cgit/lilypond.git/tree/elisp/lilypond-mode.el?h=dev/philh&id=b2ca0fefe3018477aaca23b6f672c7199ba5238e#n100

		repo.CommitURLTemplate = u.String() + "/commit/?id={{.Version}}"
		repo.FileURLTemplate = u.String() + "/tree/{{.Path}}/?id={{.Version}}"
		repo.LineFragmentTemplate = "n{{.LineNumber}}"

	case "gitweb":
		// https://gerrit.libreoffice.org/gitweb?p=online.git;a=blob;f=Makefile.am;h=cfcfd7c36fbae10e269653dc57a9b68c92d4c10b;hb=848145503bf7b98ce4a4aa0a858a0d71dd0dbb26#l10
		repo.FileURLTemplate = u.String() + ";a=blob;f={{.Path}};hb={{.Version}}"
		repo.CommitURLTemplate = u.String() + ";a=commit;h={{.Version}}"
		repo.LineFragmentTemplate = "l{{.LineNumber}}"

	default:
		return fmt.Errorf("URL scheme type %q unknown", typ)
	}
	return nil
}

// getCommit returns a tree object for the given reference.
func getCommit(repo *git.Repository, prefix, ref string) (*object.Commit, error) {
	sha1, err := repo.ResolveRevision(plumbing.Revision(ref))
	// ref might be a branch name (e.g. "master") add branch prefix and try again.
	if err != nil {
		sha1, err = repo.ResolveRevision(plumbing.Revision(filepath.Join(prefix, ref)))
	}
	if err != nil {
		return nil, err
	}

	commitObj, err := repo.CommitObject(*sha1)
	if err != nil {
		return nil, err
	}
	return commitObj, nil
}

func configLookupRemoteURL(cfg *config.Config, key string) string {
	rc := cfg.Remotes[key]
	if rc == nil || len(rc.URLs) == 0 {
		return ""
	}
	return rc.URLs[0]
}

func configLookupString(sec *plumcfg.Section, key string) string {
	for _, o := range sec.Options {
		if o.Key != key {
			continue
		}
		return o.Value
	}

	return ""
}

func isMissingBranchError(err error) bool {
	return err != nil && err.Error() == "reference not found"
}

func setTemplatesFromConfig(desc *zoekt.Repository, repoDir string) error {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return err
	}

	cfg, err := repo.Config()
	if err != nil {
		return err
	}

	sec := cfg.Raw.Section("zoekt")

	webURLStr := configLookupString(sec, "web-url")

	webURLType := configLookupString(sec, "web-url-type")

	if webURLType != "" && webURLStr != "" {
		webURL, err := url.Parse(webURLStr)
		if err != nil {
			return err
		}
		if err := setTemplates(desc, webURL, webURLType); err != nil {
			return err
		}
	}

	name := configLookupString(sec, "name")
	if name != "" {
		desc.Name = name
	} else {
		remoteURL := configLookupRemoteURL(cfg, "origin")
		if remoteURL == "" {
			return nil
		}
		u, err := url.Parse(remoteURL)
		if err != nil {
			return err
		}
		if err := SetTemplatesFromOrigin(desc, u); err != nil {
			return err
		}
	}

	if desc.RawConfig == nil {
		desc.RawConfig = map[string]string{}
	}
	for _, o := range sec.Options {
		desc.RawConfig[o.Key] = o.Value
	}

	// Ranking info.

	// Github:
	traction := 0
	for _, s := range []string{"github-stars", "github-forks", "github-watchers", "github-subscribers"} {
		f, err := strconv.Atoi(configLookupString(sec, s))
		if err == nil {
			traction += f
		}
	}

	if strings.Contains(desc.Name, "googlesource.com/") && traction == 0 {
		// Pretend everything on googlesource.com has 1000
		// github stars.
		traction = 1000
	}

	if traction > 0 {
		l := math.Log(float64(traction))
		desc.Rank = uint16((1.0 - 1.0/math.Pow(1+l, 0.6)) * 10000)
	}

	return nil
}

// SetTemplates fills in templates based on the origin URL.
func SetTemplatesFromOrigin(desc *zoekt.Repository, u *url.URL) error {
	desc.Name = filepath.Join(u.Host, strings.TrimSuffix(u.Path, ".git"))

	if strings.HasSuffix(u.Host, ".googlesource.com") {
		return setTemplates(desc, u, "gitiles")
	} else if u.Host == "github.com" {
		u.Path = strings.TrimSuffix(u.Path, ".git")
		return setTemplates(desc, u, "github")
	} else {
		return fmt.Errorf("unknown git hosting site %q", u)
	}
}

type Options struct {
	Submodules         bool
	Incremental        bool
	AllowMissingBranch bool
	RepoCacheDir       string
	BuildOptions       build.Options
	RepoDir            string

	BranchPrefix string
	Branches     []string
}

func expandBranches(repo *git.Repository, bs []string, prefix string) ([]string, error) {
	var result []string
	for _, b := range bs {
		if b == "HEAD" {
			ref, err := repo.Head()
			if err != nil {
				return nil, err
			}

			result = append(result, strings.TrimPrefix(ref.Name().String(), prefix))
			continue
		}

		if strings.Contains(b, "*") {
			iter, err := repo.Branches()
			if err != nil {
				return nil, err
			}

			defer iter.Close()
			for {
				ref, err := iter.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, err
				}

				name := ref.Name().Short()
				if matched, err := filepath.Match(b, name); err != nil {
					return nil, err
				} else if !matched {
					continue
				}

				result = append(result, strings.TrimPrefix(name, prefix))
			}
			continue
		}

		result = append(result, b)
	}

	return result, nil
}

// IndexGitRepo indexes the git repository as specified by the options.
func IndexGitRepo(opts Options) error {
	// Set max thresholds, since we use them in this function.
	opts.BuildOptions.SetDefaults()
	if opts.RepoDir == "" {
		return fmt.Errorf("gitindex: must set RepoDir")
	}
	repo, err := git.PlainOpen(opts.RepoDir)
	if err != nil {
		log.Println("B")
		return err
	}

	if err := setTemplatesFromConfig(&opts.BuildOptions.RepositoryDescription, opts.RepoDir); err != nil {
		log.Printf("setTemplatesFromConfig(%s): %s", opts.RepoDir, err)
	}

	repoCache := NewRepoCache(opts.RepoCacheDir)
	defer repoCache.Close()

	// branch => (path, sha1) => repo.
	repos := map[FileKey]BlobLocation{}

	// FileKey => branches
	branchMap := map[FileKey][]string{}

	// Branch => Repo => SHA1
	branchVersions := map[string]map[string]plumbing.Hash{}

	branches, err := expandBranches(repo, opts.Branches, opts.BranchPrefix)
	if err != nil {
		return err
	}
	for _, b := range branches {
		commit, err := getCommit(repo, opts.BranchPrefix, b)
		if opts.AllowMissingBranch && isMissingBranchError(err) {
			continue
		}

		if err != nil {
			return err
		}
		opts.BuildOptions.RepositoryDescription.Branches = append(opts.BuildOptions.RepositoryDescription.Branches, zoekt.RepositoryBranch{
			Name:    b,
			Version: commit.Hash.String(),
		})

		tree, err := commit.Tree()
		if err != nil {
			return err
		}

		files, subVersions, err := TreeToFiles(repo, tree, opts.BuildOptions.RepositoryDescription.URL, repoCache)
		if err != nil {
			return err
		}
		for k, v := range files {
			repos[k] = v
			branchMap[k] = append(branchMap[k], b)
		}

		branchVersions[b] = subVersions
	}

	if opts.Incremental {
		versions := opts.BuildOptions.IndexVersions()
		if reflect.DeepEqual(versions, opts.BuildOptions.RepositoryDescription.Branches) {
			return nil
		}
	}

	reposByPath := map[string]BlobLocation{}
	for key, location := range repos {
		reposByPath[key.SubRepoPath] = location
	}

	opts.BuildOptions.SubRepositories = map[string]*zoekt.Repository{}
	for path, location := range reposByPath {
		tpl := opts.BuildOptions.RepositoryDescription
		if path != "" {
			tpl = zoekt.Repository{URL: location.URL.String()}
			if err := SetTemplatesFromOrigin(&tpl, location.URL); err != nil {
				log.Printf("setTemplatesFromOrigin(%s, %s): %s", path, location.URL, err)
			}
		}
		opts.BuildOptions.SubRepositories[path] = &tpl
	}
	for _, br := range opts.BuildOptions.RepositoryDescription.Branches {
		for path, repo := range opts.BuildOptions.SubRepositories {
			id := branchVersions[br.Name][path]
			repo.Branches = append(repo.Branches, zoekt.RepositoryBranch{
				Name:    br.Name,
				Version: id.String(),
			})
		}
	}

	builder, err := build.NewBuilder(opts.BuildOptions)
	if err != nil {
		return err
	}

	var names []string
	fileKeys := map[string][]FileKey{}
	for key := range repos {
		n := key.FullPath()
		fileKeys[n] = append(fileKeys[n], key)
		names = append(names, n)
	}
	// not strictly necessary, but nice for reproducibility.
	sort.Strings(names)

	for _, name := range names {
		keys := fileKeys[name]
		for _, key := range keys {
			brs := branchMap[key]
			blob, err := repos[key].Repo.BlobObject(key.ID)
			if err != nil {
				return err
			}

			if blob.Size > int64(opts.BuildOptions.SizeMax) {
				continue
			}

			contents, err := blobContents(blob)
			if err != nil {
				return err
			}
			builder.Add(zoekt.Document{
				SubRepositoryPath: key.SubRepoPath,
				Name:              key.FullPath(),
				Content:           contents,
				Branches:          brs,
			})
		}
	}
	return builder.Finish()
}

func blobContents(blob *object.Blob) ([]byte, error) {
	r, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	c, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return c, nil
}
