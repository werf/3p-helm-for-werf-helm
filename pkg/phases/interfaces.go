package phases

import (
	"github.com/werf/3p-helm-for-werf-helm/pkg/kube"
	"github.com/werf/3p-helm-for-werf-helm/pkg/phases/stages"
)

type Splitter interface {
	Split(resources kube.ResourceList) (stages.SortedStageList, error)
}

type ExternalDepsGenerator interface {
	Generate(stages stages.SortedStageList) error
}
