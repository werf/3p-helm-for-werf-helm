package phases

import (
	"fmt"

	"k8s.io/cli-runtime/pkg/resource"

	"github.com/werf/3p-helm-for-werf-helm/pkg/kube"
	"github.com/werf/3p-helm-for-werf-helm/pkg/phases/stages"
)

type SingleStageSplitter struct{}

func (s *SingleStageSplitter) Split(resources kube.ResourceList) (stages.SortedStageList, error) {
	stage := &stages.Stage{}

	if err := resources.Visit(func(res *resource.Info, err error) error {
		if err != nil {
			return err
		}

		stage.DesiredResources.Append(res)

		return nil
	}); err != nil {
		return nil, fmt.Errorf("error visiting resources list: %w", err)
	}

	return stages.SortedStageList{stage}, nil
}
