/*
Copyright The Helm Authors.

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

package action

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/werf/3p-helm-for-werf-helm/pkg/chartutil"
	"github.com/werf/3p-helm-for-werf-helm/pkg/kube"
	"github.com/werf/3p-helm-for-werf-helm/pkg/phases"
	"github.com/werf/3p-helm-for-werf-helm/pkg/release"
	"github.com/werf/3p-helm-for-werf-helm/pkg/releaseutil"
	"github.com/werf/3p-helm-for-werf-helm/pkg/storage/driver"
	helmtime "github.com/werf/3p-helm-for-werf-helm/pkg/time"
)

// Uninstall is the action for uninstalling releases.
//
// It provides the implementation of 'helm uninstall'.
type Uninstall struct {
	cfg *Configuration

	DisableHooks        bool
	DryRun              bool
	IgnoreNotFound      bool
	KeepHistory         bool
	Wait                bool
	DeletionPropagation string
	Timeout             time.Duration
	Description         string

	DeleteHooks     bool
	DeleteNamespace bool
	Namespace       string
	StagesSplitter  phases.Splitter
}

// NewUninstall creates a new Uninstall object with the given configuration.
func NewUninstall(cfg *Configuration, stagesSplitter phases.Splitter) *Uninstall {
	if stagesSplitter == nil {
		stagesSplitter = &phases.SingleStageSplitter{}
	}

	return &Uninstall{
		cfg: cfg,

		StagesSplitter: stagesSplitter,
	}
}

// Run uninstalls the given release.
func (u *Uninstall) Run(name string) (*release.UninstallReleaseResponse, error) {
	if err := u.cfg.KubeClient.IsReachable(); err != nil {
		return nil, err
	}

	if u.DryRun {
		// In the dry run case, just see if the release exists
		r, err := u.cfg.releaseContent(name, 0)
		if err != nil {
			if apierrors.IsNotFound(err) && u.IgnoreNotFound {
				u.cfg.Log("No such release %q", name)
				return &release.UninstallReleaseResponse{}, nil
			}

			return &release.UninstallReleaseResponse{}, err
		}
		return &release.UninstallReleaseResponse{Release: r}, nil
	}

	if err := chartutil.ValidateReleaseName(name); err != nil {
		return nil, errors.Errorf("uninstall: Release name is invalid: %s", name)
	}

	rels, err := u.cfg.Releases.History(name)
	if err != nil {
		if u.IgnoreNotFound && errors.Is(err, driver.ErrReleaseNotFound) {
			u.cfg.Log("No such release %q", name)

			if u.DeleteNamespace && !u.KeepHistory {
				if err := u.cfg.KubeClient.DeleteNamespace(context.Background(), u.Namespace, kube.DeleteOptions{Wait: true, WaitTimeout: u.Timeout}); err != nil {
					if kube.IsNotFound(err) {
						u.cfg.Log("No such namespace %q", u.Namespace)
						return &release.UninstallReleaseResponse{}, nil
					}
					return nil, errors.Wrapf(err, "unable to delete namespace %s", u.Namespace)
				}
			}

			return nil, nil
		}
		return nil, errors.Wrapf(err, "uninstall: Release not loaded: %s", name)
	}
	if len(rels) < 1 {
		return nil, errMissingRelease
	}

	releaseutil.SortByRevision(rels)
	rel := rels[len(rels)-1]

	// TODO: Are there any cases where we want to force a delete even if it's
	// already marked deleted?
	if rel.Info.Status == release.StatusUninstalled {
		if u.IgnoreNotFound {
			return &release.UninstallReleaseResponse{Release: rel}, nil
		}
		if !u.KeepHistory {
			if err := u.purgeReleases(rels...); err != nil {
				return nil, errors.Wrap(err, "uninstall: Failed to purge the release")
			}
			return &release.UninstallReleaseResponse{Release: rel}, nil
		}
		return nil, errors.Errorf("the release named %q is already deleted", name)
	}

	u.cfg.Log("uninstall: Deleting %s", name)
	rel.Info.Status = release.StatusUninstalling
	rel.Info.Deleted = helmtime.Now()
	rel.Info.Description = "Deletion in progress (or silently failed)"
	res := &release.UninstallReleaseResponse{Release: rel}

	if !u.DisableHooks {
		if err := u.cfg.execHook(rel, release.HookPreDelete, u.Timeout); err != nil {
			return res, err
		}
	} else {
		u.cfg.Log("delete hooks disabled for %s", name)
	}

	deployedResourcesCalculator := phases.NewDeployedResourcesCalculator(rels, u.StagesSplitter, u.cfg.KubeClient)
	deployedResources, err := deployedResourcesCalculator.Calculate()
	if err != nil {
		return nil, fmt.Errorf("error calculating deployed resources: %w", err)
	}

	release.SetUninstallPhaseStageInfo(rel)

	// From here on out, the release is currently considered to be in StatusUninstalling
	// state.
	if err := u.cfg.Releases.Update(rel); err != nil {
		u.cfg.Log("uninstall: Failed to store updated release: %s", err)
	}

	deletedResources, kept, errs := u.deleteRelease(rel, deployedResources)
	if errs != nil {
		u.cfg.Log("uninstall: Failed to delete release: %s", errs)
		return nil, errors.Errorf("failed to delete release: %s", name)
	}

	if kept != "" {
		kept = "These resources were kept due to the resource policy:\n" + kept
	}
	res.Info = kept

	if u.Wait {
		if kubeClient, ok := u.cfg.KubeClient.(kube.InterfaceExt); ok {
			if err := kubeClient.WaitForDelete(deletedResources, u.Timeout); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if !u.DisableHooks {
		if err := u.cfg.execHook(rel, release.HookPostDelete, u.Timeout); err != nil {
			errs = append(errs, err)
		}
	}

	if u.DeleteHooks {
		var hooksFromAllReleases []*release.Hook
		for _, r := range rels {
			hooksFromAllReleases = append(hooksFromAllReleases, r.Hooks...)
		}
		if len(hooksFromAllReleases) > 0 {
			if err := u.cfg.deleteHooks(hooksFromAllReleases); err != nil {
				errs = append(errs, err)
			}
		}
	}

	rel.Info.Status = release.StatusUninstalled
	if len(u.Description) > 0 {
		rel.Info.Description = u.Description
	} else {
		rel.Info.Description = "Uninstallation complete"
	}

	if !u.KeepHistory {
		u.cfg.Log("purge requested for %s", name)
		err := u.purgeReleases(rels...)
		if err != nil {
			errs = append(errs, errors.Wrap(err, "uninstall: Failed to purge the release"))
		}

		// Return the errors that occurred while deleting the release, if any
		if len(errs) > 0 {
			return res, errors.Errorf("uninstallation completed with %d error(s): %s", len(errs), joinErrors(errs))
		}

		if u.DeleteNamespace {
			if err := u.cfg.KubeClient.DeleteNamespace(context.Background(), u.Namespace, kube.DeleteOptions{Wait: true, WaitTimeout: u.Timeout}); err != nil {
				return res, errors.Wrapf(err, "unable to delete namespace %s", u.Namespace)
			}
		}

		return res, nil
	}

	if err := u.cfg.Releases.Update(rel); err != nil {
		u.cfg.Log("uninstall: Failed to store updated release: %s", err)
	}

	if len(errs) > 0 {
		return res, errors.Errorf("uninstallation completed with %d error(s): %s", len(errs), joinErrors(errs))
	}
	return res, nil
}

func (u *Uninstall) purgeReleases(rels ...*release.Release) error {
	for _, rel := range rels {
		if _, err := u.cfg.Releases.Delete(rel.Name, rel.Version); err != nil {
			return err
		}
	}
	return nil
}

func joinErrors(errs []error) string {
	es := make([]string, 0, len(errs))
	for _, e := range errs {
		es = append(es, e.Error())
	}
	return strings.Join(es, "; ")
}

// deleteRelease deletes the release and returns list of delete resources and manifests that were kept in the deletion process
func (u *Uninstall) deleteRelease(rel *release.Release, res kube.ResourceList) (kube.ResourceList, string, []error) {
	manifestsStr, err := res.ToYamlDocs()
	if err != nil {
		return nil, "", []error{fmt.Errorf("error converting resource list to yaml manifests: %w", err)}
	}

	var errs []error
	caps, err := u.cfg.getCapabilities()
	if err != nil {
		return nil, manifestsStr, []error{errors.Wrap(err, "could not get apiVersions from Kubernetes")}
	}

	manifests := releaseutil.SplitManifests(manifestsStr)
	_, files, err := releaseutil.SortManifests(manifests, caps.APIVersions, releaseutil.UninstallOrder)
	if err != nil {
		// We could instead just delete everything in no particular order.
		// FIXME: One way to delete at this point would be to try a label-based
		// deletion. The problem with this is that we could get a false positive
		// and delete something that was not legitimately part of this release.
		return nil, manifestsStr, []error{errors.Wrap(err, "corrupted release record. You must manually delete the resources")}
	}

	filesToKeep, filesToDelete := filterManifestsToKeep(files)
	var kept string
	for _, f := range filesToKeep {
		kept += "[" + f.Head.Kind + "] " + f.Head.Metadata.Name + "\n"
	}

	var builder strings.Builder
	for _, file := range filesToDelete {
		builder.WriteString("\n---\n" + file.Content)
	}

	resources, err := u.cfg.KubeClient.Build(strings.NewReader(builder.String()), false)
	if err != nil {
		return nil, "", []error{errors.Wrap(err, "unable to build kubernetes objects for delete")}
	}
	if len(resources) > 0 {
		if kubeClient, ok := u.cfg.KubeClient.(kube.InterfaceDeletionPropagation); ok {
			_, errs = kubeClient.DeleteWithPropagationPolicy(resources, parseCascadingFlag(u.cfg, u.DeletionPropagation), kube.DeleteOptions{
				Wait:                   true,
				SkipIfInvalidOwnership: true,
				ReleaseName:            rel.Name,
				ReleaseNamespace:       rel.Namespace,
			})
			return resources, kept, errs
		}

		_, errs = u.cfg.KubeClient.Delete(resources, kube.DeleteOptions{
			Wait:                   true,
			SkipIfInvalidOwnership: true,
			ReleaseName:            rel.Name,
			ReleaseNamespace:       rel.Namespace,
		})
	}
	return resources, kept, errs
}

func parseCascadingFlag(cfg *Configuration, cascadingFlag string) v1.DeletionPropagation {
	switch cascadingFlag {
	case "orphan":
		return v1.DeletePropagationOrphan
	case "foreground":
		return v1.DeletePropagationForeground
	case "background":
		return v1.DeletePropagationBackground
	default:
		cfg.Log("uninstall: given cascade value: %s, defaulting to delete propagation background", cascadingFlag)
		return v1.DeletePropagationBackground
	}
}
