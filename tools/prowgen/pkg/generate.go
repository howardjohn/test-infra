// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

import (
	"fmt"
	"io/ioutil"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/imdario/mergo"
	"gopkg.in/robfig/cron.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	prowjob "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/gerrit/client"
	"sigs.k8s.io/yaml"

	"istio.io/test-infra/tools/prowgen/pkg/decorator"
	"istio.io/test-infra/tools/prowgen/pkg/spec"
)

const (
	TestGridDashboard   = "testgrid-dashboards"
	TestGridAlertEmail  = "testgrid-alert-email"
	TestGridNumFailures = "testgrid-num-failures-to-alert"

	DefaultAutogenHeader = "# THIS FILE IS AUTOGENERATED, DO NOT EDIT IT MANUALLY."

	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"

	TypePostsubmit = "postsubmit"
	TypePresubmit  = "presubmit"
	TypePeriodic   = "periodic"

	// Kubernetes has a label limit of 63 characters
	maxJobNameLength = 63
)

type Client struct {
	BaseConfig spec.BaseConfig

	LongJobNamesAllowed bool
}

func ReadBase(baseConfig *spec.BaseConfig, file string) spec.BaseConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		log.Fatalf("Failed to read %q: %v", file, err)
	}
	newBaseConfig := spec.BaseConfig{}
	if err := yaml.UnmarshalStrict(yamlFile, &newBaseConfig, yaml.DisallowUnknownFields); err != nil {
		log.Fatalf("Failed to unmarshal %q: %v", file, err)
	}
	if baseConfig == nil {
		return newBaseConfig
	}

	mergedBaseConfig := baseConfig.DeepCopy()
	mergedBaseConfig.CommonConfig = mergeCommonConfig(mergedBaseConfig.CommonConfig, newBaseConfig.CommonConfig)

	return mergedBaseConfig
}

// Reads the jobs yaml
func (cli *Client) ReadJobsConfig(file string) spec.JobsConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		log.Fatalf("Failed to read %q: %v", file, err)
	}
	jobsConfig := spec.JobsConfig{}
	if err := yaml.UnmarshalStrict(yamlFile, &jobsConfig); err != nil {
		log.Fatalf("Failed to unmarshal %q: %v", file, err)
	}

	if len(jobsConfig.Branches) == 0 {
		jobsConfig.Branches = []string{"master"}
	}

	return resolveOverwrites(cli.BaseConfig.CommonConfig.DeepCopy(), jobsConfig)
}

func deepCopyMap(mp map[string]string) map[string]string {
	bs, _ := yaml.Marshal(mp)
	newMap := map[string]string{}
	if err := yaml.Unmarshal(bs, &newMap); err != nil {
		log.Fatalf("Failed to unmarshal Map: %v", err)
	}
	return newMap
}

func mergeCommonConfig(configs ...spec.CommonConfig) spec.CommonConfig {
	mergedCommonConfig := spec.CommonConfig{}
	for i := 0; i < len(configs); i++ {
		config := configs[i].DeepCopy()
		if err := mergo.Merge(&mergedCommonConfig, config,
			mergo.WithAppendSlice, mergo.WithSliceDeepCopy); err != nil {
			log.Fatalf("Failed to merge config: %v", err)
		}

		// NodeSelector field is a special case since for Prow jobs we normally only
		// want to schedule them on dedicated nodes that only matches with one
		// single label.
		if len(configs[i].NodeSelector) != 0 {
			mergedCommonConfig.NodeSelector = deepCopyMap(configs[i].NodeSelector)
		}
	}
	return mergedCommonConfig
}

func resolveOverwrites(baseCommonConfig spec.CommonConfig, jobsConfig spec.JobsConfig) spec.JobsConfig {
	jobsConfig.CommonConfig = mergeCommonConfig(baseCommonConfig, jobsConfig.CommonConfig)

	for i, job := range jobsConfig.Jobs {
		job.CommonConfig = mergeCommonConfig(jobsConfig.CommonConfig, job.CommonConfig)

		jobsConfig.Jobs[i] = job
	}

	return jobsConfig
}

// FilterReleaseBranchingJobs filters then returns jobs with release branching enabled.
func FilterReleaseBranchingJobs(jobs []spec.Job) []spec.Job {
	jobsF := make([]spec.Job, 0)
	for _, j := range jobs {
		if j.DisableReleaseBranching {
			continue
		}
		jobsF = append(jobsF, j)
	}
	return jobsF
}

func validateJobsConfig(fileName string, jobsConfig spec.JobsConfig) error {
	var err error
	if jobsConfig.Org == "" {
		err = multierror.Append(err, fmt.Errorf("%s: org must be set", fileName))
	}
	if jobsConfig.Repo == "" {
		err = multierror.Append(err, fmt.Errorf("%s: repo must be set", fileName))
	}

	for _, job := range jobsConfig.Jobs {
		if jobsConfig.Org == "istio" || jobsConfig.Org == "istio-private" {
			// Some other orgs may have other naming conventions, but for Istio we use _ as divider between job
			// name, repo, and type. So exclude it from the name.
			if strings.Contains(job.Name, "_") {
				err = multierror.Append(err, fmt.Errorf("%s: job may not contain '_' %v", fileName, job.Name))
			}
		}
		if job.Image == "" {
			err = multierror.Append(err, fmt.Errorf("%s: image must be set for job %v", fileName, job.Name))
		}
		if job.Resources != "" {
			if _, f := jobsConfig.ResourcePresets[job.Resources]; !f {
				err = multierror.Append(err, fmt.Errorf("%s: job '%v' has nonexistant resource '%v'", fileName, job.Name, job.Resources))
			}
		}

		if sets.NewString(job.Types...).Has(TypePeriodic) {
			if job.Cron != "" && job.Interval != "" {
				err = multierror.Append(err, fmt.Errorf("%s: cron and interval cannot be both set in periodic %s", fileName, job.Name))
			} else if job.Cron == "" && job.Interval == "" {
				err = multierror.Append(err, fmt.Errorf("%s: cron and interval cannot be both empty in periodic %s", fileName, job.Name))
			} else if job.Cron != "" {
				if _, e := cron.Parse(job.Cron); e != nil {
					err = multierror.Append(err, fmt.Errorf("%s: invalid cron string %s in periodic %s: %v", fileName, job.Cron, job.Name, e))
				}
			} else if job.Interval != "" {
				if _, e := time.ParseDuration(job.Interval); e != nil {
					err = multierror.Append(err, fmt.Errorf("%s: cannot parse duration %s in periodic %s: %v", fileName, job.Interval, job.Name, e))
				}
			}
		}
		for _, t := range job.Types {
			if e := validate(t, sets.NewString(TypePostsubmit, TypePresubmit, TypePeriodic), "type"); e != nil {
				err = multierror.Append(err, e)
			}
		}
		for _, t := range job.Architectures {
			if e := validate(t, sets.NewString(ArchAMD64, ArchARM64, TypePeriodic), "architectures"); e != nil {
				err = multierror.Append(err, e)
			}
		}
		for _, repo := range job.Repos {
			if len(strings.Split(repo, "/")) != 2 {
				err = multierror.Append(err, fmt.Errorf("%s: repo %v not valid, should take form org/repo", fileName, repo))
			}
		}
	}

	return err
}

func (cli *Client) ConvertJobConfig(fileName string, jobsConfig spec.JobsConfig, branch string) (config.JobConfig, error) {
	output := config.JobConfig{
		PresubmitsStatic:  map[string][]config.Presubmit{},
		PostsubmitsStatic: map[string][]config.Postsubmit{},
		Periodics:         []config.Periodic{},
	}
	if err := validateJobsConfig(fileName, jobsConfig); err != nil {
		return output, err
	}

	baseConfig := cli.BaseConfig
	testgridConfig := baseConfig.TestgridConfig

	var presubmits []config.Presubmit
	var postsubmits []config.Postsubmit
	var periodics []config.Periodic

	for _, parentJob := range jobsConfig.Jobs {
		if len(parentJob.Architectures) == 0 {
			parentJob.Architectures = []string{ArchAMD64}
		}

		expandedJobs := decorator.ApplyVariables(parentJob, parentJob.Architectures, jobsConfig.Params, jobsConfig.Matrix, cli.BaseConfig.ClusterOverrides)
		for _, job := range expandedJobs {
			brancher := config.Brancher{
				Branches: []string{fmt.Sprintf("^%s$", branch)},
			}

			testgridJobPrefix := jobsConfig.Org
			if branch != "master" {
				testgridJobPrefix += "_" + branch
			}
			testgridJobPrefix += "_" + jobsConfig.Repo

			if len(job.Types) == 0 || sets.NewString(job.Types...).Has(TypePresubmit) {
				name := fmt.Sprintf("%s_%s", job.Name, jobsConfig.Repo)
				if branch != "master" {
					name += "_" + branch
				}

				base, err := cli.createJobBase(baseConfig, jobsConfig, job, name, branch, jobsConfig.ResourcePresets)
				if err != nil {
					return output, err
				}

				presubmit := config.Presubmit{
					JobBase:   base,
					AlwaysRun: true,
					Brancher:  brancher,
				}
				if job.GerritPresubmitLabel != "" {
					presubmit.Labels[client.GerritReportLabel] = job.GerritPresubmitLabel
				}
				if pa, ok := baseConfig.PathAliases[jobsConfig.Org]; ok {
					presubmit.UtilityConfig.PathAlias = fmt.Sprintf("%s/%s", pa, jobsConfig.Repo)
				}
				if job.Regex != "" {
					presubmit.RegexpChangeMatcher = config.RegexpChangeMatcher{
						RunIfChanged: job.Regex,
					}
					presubmit.AlwaysRun = false
				}
				triggers := []string{
					// Allow "/test job"
					"(" + config.DefaultTriggerFor(job.Name) + ")",
					// Allow "/test job_repo_branch"
					"(" + config.DefaultTriggerFor(name) + ")",
				}
				if job.Trigger != "" {
					// Allow custom trigger
					triggers = append(triggers, fmt.Sprintf(`((?m)^%s(\s+|$))`, job.Trigger))
				}
				presubmit.Trigger = strings.Join(triggers, `|`)
				presubmit.RerunCommand = fmt.Sprintf("/test %s", job.Name)
				if testgridConfig.Enabled {
					if err := mergo.Merge(&presubmit.JobBase.Annotations, map[string]string{
						TestGridDashboard: testgridJobPrefix,
					}); err != nil {
						return output, err
					}
				}
				decorator.ApplyModifiersPresubmit(&presubmit, job.Modifiers)
				decorator.ApplyRequirements(&presubmit.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
				presubmits = append(presubmits, presubmit)
			}

			if len(job.Types) == 0 || sets.NewString(job.Types...).Has(TypePostsubmit) {
				name := fmt.Sprintf("%s_%s", job.Name, jobsConfig.Repo)
				if branch != "master" {
					name += "_" + branch
				}
				name += "_postsubmit"

				base, err := cli.createJobBase(baseConfig, jobsConfig, job, name, branch, jobsConfig.ResourcePresets)
				if err != nil {
					return output, err
				}

				postsubmit := config.Postsubmit{
					JobBase:  base,
					Brancher: brancher,
				}
				if job.GerritPostsubmitLabel != "" {
					postsubmit.Labels[client.GerritReportLabel] = job.GerritPostsubmitLabel
				}
				if pa, ok := baseConfig.PathAliases[jobsConfig.Org]; ok {
					postsubmit.UtilityConfig.PathAlias = fmt.Sprintf("%s/%s", pa, jobsConfig.Repo)
				}
				if job.Regex != "" {
					postsubmit.RegexpChangeMatcher = config.RegexpChangeMatcher{
						RunIfChanged: job.Regex,
					}
				}
				if testgridConfig.Enabled {
					if err := mergo.Merge(&postsubmit.JobBase.Annotations, map[string]string{
						TestGridDashboard:   testgridJobPrefix + "_postsubmit",
						TestGridAlertEmail:  testgridConfig.AlertEmail,
						TestGridNumFailures: testgridConfig.NumFailuresToAlert,
					}); err != nil {
						return output, err
					}
				}
				decorator.ApplyModifiersPostsubmit(&postsubmit, job.Modifiers)
				decorator.ApplyRequirements(&postsubmit.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
				postsubmits = append(postsubmits, postsubmit)
			}

			if sets.NewString(job.Types...).Has(TypePeriodic) {
				name := fmt.Sprintf("%s_%s", job.Name, jobsConfig.Repo)
				if branch != "master" {
					name += "_" + branch
				}
				name += "_periodic"

				// For periodic jobs, the repo needs to be added to the clonerefs and its root directory
				// should be set as the working directory, so add itself to the repo list here.
				job.Repos = append([]string{jobsConfig.Org + "/" + jobsConfig.Repo}, job.Repos...)

				base, err := cli.createJobBase(baseConfig, jobsConfig, job, name, branch, jobsConfig.ResourcePresets)
				if err != nil {
					return output, err
				}
				periodic := config.Periodic{
					JobBase:  base,
					Interval: job.Interval,
					Cron:     job.Cron,
					Tags:     job.Tags,
				}
				for _, requirement := range job.Requirements {
					if cronstr := jobsConfig.RequirementPresets[requirement].Cron; cronstr != "" {
						periodic.Cron = cronstr
					}
				}
				if testgridConfig.Enabled {
					if err := mergo.Merge(&periodic.JobBase.Annotations, map[string]string{
						TestGridDashboard:   testgridJobPrefix + "_periodic",
						TestGridAlertEmail:  testgridConfig.AlertEmail,
						TestGridNumFailures: testgridConfig.NumFailuresToAlert,
					}); err != nil {
						return output, err
					}
				}
				decorator.ApplyRequirements(&periodic.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
				periodics = append(periodics, periodic)
			}
		}

		if len(presubmits) > 0 {
			output.PresubmitsStatic[fmt.Sprintf("%s/%s", jobsConfig.Org, jobsConfig.Repo)] = presubmits
		}
		if len(postsubmits) > 0 {
			output.PostsubmitsStatic[fmt.Sprintf("%s/%s", jobsConfig.Org, jobsConfig.Repo)] = postsubmits
		}
		if len(periodics) > 0 {
			output.Periodics = periodics
		}
	}
	return output, nil
}

func createContainer(jobConfig spec.JobsConfig, job spec.Job, resources map[string]v1.ResourceRequirements) []v1.Container {
	envs := joinEnv(jobConfig.Env, job.Env)

	yes := true
	c := v1.Container{
		Image:           job.Image,
		SecurityContext: &v1.SecurityContext{Privileged: &yes},
		Command:         job.Command,
		Args:            job.Args,
		Env:             envs,
	}
	if job.ImagePullPolicy != "" {
		c.ImagePullPolicy = v1.PullPolicy(job.ImagePullPolicy)
	}

	decorator.ApplyResource(&c, job.Resources, resources)

	return []v1.Container{c}
}

// joinEnv joins a set of environment variables, in order of lowest to highest priority
func joinEnv(envs ...[]v1.EnvVar) []v1.EnvVar {
	envMap := map[string]interface{}{}
	for _, es := range envs {
		for _, e := range es {
			if e.ValueFrom != nil {
				envMap[e.Name] = e.ValueFrom
			} else {
				envMap[e.Name] = e.Value
			}
		}
	}
	res := []v1.EnvVar{}
	for k, v := range envMap {
		switch tv := v.(type) {
		case string:
			res = append(res, v1.EnvVar{Name: k, Value: tv})
		case *v1.EnvVarSource:
			res = append(res, v1.EnvVar{Name: k, ValueFrom: tv})
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].Name < res[j].Name
	})
	return res
}

func (cli *Client) createJobBase(baseConfig spec.BaseConfig, jobConfig spec.JobsConfig, job spec.Job,
	name string, branch string, resources map[string]v1.ResourceRequirements) (config.JobBase, error,
) {
	if len(name) > maxJobNameLength && !cli.LongJobNamesAllowed {
		return config.JobBase{}, fmt.Errorf("job name exceeds %v character limit '%v'", maxJobNameLength, name)
	}

	yes := true
	no := false
	jb := config.JobBase{
		Name:           name,
		MaxConcurrency: job.MaxConcurrency,
		Spec: &v1.PodSpec{
			Containers:   createContainer(jobConfig, job, resources),
			NodeSelector: job.NodeSelector,
			// Disable mounting the service account token. None of our jobs should ever be connecting to the API server.
			// We do use service accounts, but only for GKE workload identity which doesn't require this.
			// Aside from security concerns, this also triggers https://github.com/kubernetes/kubernetes/issues/99884 which
			// ends up with `kubectl` being confused about which namespace it should be.
			AutomountServiceAccountToken: &no,
		},
		UtilityConfig: config.UtilityConfig{
			Decorate:  &yes,
			ExtraRefs: createExtraRefs(job.Repos, branch, baseConfig.PathAliases),
		},
		ReporterConfig: job.ReporterConfig,
		Labels:         job.Labels,
		Annotations:    job.Annotations,
		Cluster:        job.Cluster,
	}
	if arch, f := job.NodeSelector[v1.LabelArchStable]; f && arch != ArchAMD64 {
		// Support https://cloud.google.com/kubernetes-engine/docs/how-to/prepare-arm-workloads-for-deployment#multi-arch-schedule-any-arch
		// Not all clusters may need this, but it doesn't hurt to add it.
		jb.Spec.Tolerations = append(jb.Spec.Tolerations, v1.Toleration{
			Key:      v1.LabelArchStable,
			Operator: v1.TolerationOpEqual,
			Value:    arch,
			Effect:   v1.TaintEffectNoSchedule,
		})
	}
	if len(job.ImagePullSecrets) != 0 {
		jb.Spec.ImagePullSecrets = make([]v1.LocalObjectReference, 0)
		for _, ips := range job.ImagePullSecrets {
			jb.Spec.ImagePullSecrets = append(jb.Spec.ImagePullSecrets, v1.LocalObjectReference{Name: ips})
		}
	}

	if jb.Labels == nil {
		jb.Labels = map[string]string{}
	}
	if jb.Annotations == nil {
		jb.Annotations = map[string]string{}
	}

	if job.ServiceAccountName != "" {
		jb.Spec.ServiceAccountName = job.ServiceAccountName
	}

	if job.TerminationGracePeriodSeconds != 0 {
		jb.Spec.TerminationGracePeriodSeconds = &job.TerminationGracePeriodSeconds
	}

	if job.Timeout != nil {
		if jb.DecorationConfig == nil {
			jb.DecorationConfig = &prowjob.DecorationConfig{}
		}
		jb.DecorationConfig.Timeout = job.Timeout
	}
	if job.GCSLogBucket != "" {
		if jb.DecorationConfig == nil {
			jb.DecorationConfig = &prowjob.DecorationConfig{}
		}
		jb.DecorationConfig.GCSConfiguration = &prowjob.GCSConfiguration{
			Bucket:       job.GCSLogBucket,
			PathStrategy: "explicit",
		}
	}

	return jb, nil
}

func createExtraRefs(extraRepos []string, defaultBranch string, pathAliases map[string]string) []prowjob.Refs {
	refs := make([]prowjob.Refs, 0)
	for _, extraRepo := range extraRepos {
		branch := defaultBranch
		repobranch := strings.Split(extraRepo, "@")
		if len(repobranch) > 1 {
			branch = repobranch[1]
		}
		orgrepo := repobranch[0]
		repo := orgrepo[strings.LastIndex(orgrepo, "/")+1:]
		org := strings.TrimSuffix(orgrepo, "/"+repo)
		ref := prowjob.Refs{
			Org:     org,
			Repo:    repo,
			BaseRef: branch,
		}

		if pa, ok := pathAliases[org]; ok {
			ref.PathAlias = fmt.Sprintf("%s/%s", pa, repo)
		}
		// If the org name contains '.', it's assumed to be a Gerrit org, since
		// '.' is not allowed in GitHub org names.
		// For Gerrit repos, the clone_uri should be always set as https://org/repo
		if strings.Contains(org, ".") {
			ref.CloneURI = "https://" + orgrepo
		}
		refs = append(refs, ref)
	}
	return refs
}

func validate(input string, options sets.String, description string) error {
	if !options.Has(input) {
		return fmt.Errorf("'%v' is not a valid %v. Must be one of %v", input, description, strings.Join(options.List(), ", "))
	}
	return nil
}
