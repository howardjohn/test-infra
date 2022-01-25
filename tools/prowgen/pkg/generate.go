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
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/hashicorp/go-multierror"
	"github.com/imdario/mergo"
	"github.com/kr/pretty"
	"gopkg.in/robfig/cron.v2"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	prowjob "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/gerrit/client"
	"sigs.k8s.io/yaml"
)

func exit(err error, context string) {
	if context == "" {
		_, _ = fmt.Fprintf(os.Stderr, "%v\n", err)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "%v: %v\n", context, err)
	}
	os.Exit(1)
}

const (
	TestGridDashboard   = "testgrid-dashboards"
	TestGridAlertEmail  = "testgrid-alert-email"
	TestGridNumFailures = "testgrid-num-failures-to-alert"

	DefaultAutogenHeader = "# THIS FILE IS AUTOGENERATED, DO NOT EDIT IT MANUALLY."

	DefaultResource = "default"

	ModifierHidden            = "hidden"
	ModifierPresubmitOptional = "presubmit_optional"
	ModifierPresubmitSkipped  = "presubmit_skipped"

	TypePostsubmit = "postsubmit"
	TypePresubmit  = "presubmit"
	TypePeriodic   = "periodic"

	variableSubstitutionFormat = `\$\([_a-zA-Z0-9.-]+(\.[_a-zA-Z0-9.-]+)*\)`

	// Kubernetes has a label limit of 63 characters
	maxJobNameLength = 63
)

var variableSubstitutionRegex = regexp.MustCompile(variableSubstitutionFormat)

type Client struct {
	BaseConfig BaseConfig

	LongJobNamesAllowed bool
}

type BaseConfig struct {
	CommonConfig

	AutogenHeader string `json:"autogen_header,omitempty"`

	PathAliases map[string]string `json:"path_aliases,omitempty"`

	TestgridConfig TestgridConfig `json:"testgrid_config,omitempty"`
}

type TestgridConfig struct {
	Enabled            bool   `json:"enabled,omitempty"`
	AlertEmail         string `json:"alert_email,omitempty"`
	NumFailuresToAlert string `json:"num_failures_to_alert,omitempty"`
}

type JobsConfig struct {
	CommonConfig

	SupportReleaseBranching bool `json:"support_release_branching,omitempty"`

	Repo     string   `json:"repo,omitempty"`
	Org      string   `json:"org,omitempty"`
	CloneURI string   `json:"clone_uri,omitempty"`
	Branches []string `json:"branches,omitempty"`

	Matrix map[string][]string `json:"matrix,omitempty"`

	Jobs []Job `json:"jobs,omitempty"`
}

type Job struct {
	CommonConfig

	DisableReleaseBranching bool `json:"disable_release_branching,omitempty"`

	Name    string   `json:"name,omitempty"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Types   []string `json:"types,omitempty"`
	Repos   []string `json:"repos,omitempty"`

	GerritPresubmitLabel  string `json:"gerrit_presubmit_label,omitempty"`
	GerritPostsubmitLabel string `json:"gerrit_postsubmit_label,omitempty"`

	ReporterConfig *prowjob.ReporterConfig `json:"reporter_config,omitempty"`
}

type CommonConfig struct {
	GCSLogBucket                  string `json:"gcs_log_bucket,omitempty"`
	TerminationGracePeriodSeconds int64  `json:"termination_grace_period_seconds,omitempty"`

	Interval string `json:"interval,omitempty"`
	Cron     string `json:"cron,omitempty"`

	Cluster      string            `json:"cluster,omitempty"`
	NodeSelector map[string]string `json:"node_selector,omitempty"`

	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`

	ResourcePresets      map[string]v1.ResourceRequirements `json:"resources_presets,omitempty"`
	RequirementPresets   map[string]RequirementPreset       `json:"requirement_presets,omitempty"`
	Requirements         []string                           `json:"requirements,omitempty"`
	ExcludedRequirements []string                           `json:"excluded_requirements,omitempty"`

	Env                []v1.EnvVar `json:"env,omitempty"`
	Image              string      `json:"image,omitempty"`
	ImagePullPolicy    string      `json:"image_pull_policy,omitempty"`
	ImagePullSecrets   []string    `json:"image_pull_secrets,omitempty"`
	ServiceAccountName string      `json:"service_account_name,omitempty"`

	Regex   string `json:"regex,omitempty"`
	Trigger string `json:"trigger,omitempty"`

	Timeout        *prowjob.Duration `json:"timeout,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`

	Resources string   `json:"resources,omitempty"`
	Modifiers []string `json:"modifiers,omitempty"`
}

func ReadBase(baseConfig *BaseConfig, file string) *BaseConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		exit(err, "failed to read "+file)
	}
	newBaseConfig := &BaseConfig{}
	if err := yaml.UnmarshalStrict(yamlFile, newBaseConfig, yaml.DisallowUnknownFields); err != nil {
		exit(err, "failed to unmarshal "+file)
	}
	if baseConfig == nil {
		return newBaseConfig
	}

	mergedBaseConfig := deepCopyBaseConfig(*baseConfig)
	mergedBaseConfig.CommonConfig = mergeCommonConfig(mergedBaseConfig.CommonConfig, newBaseConfig.CommonConfig)

	return &mergedBaseConfig
}

// Reads the jobs yaml
func (cli *Client) ReadJobsConfig(file string) JobsConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		exit(err, "failed to read "+file)
	}
	jobsConfig := JobsConfig{}
	if err := yaml.Unmarshal(yamlFile, &jobsConfig); err != nil {
		exit(err, "failed to unmarshal "+file)
	}

	if len(jobsConfig.Branches) == 0 {
		jobsConfig.Branches = []string{"master"}
	}

	return resolveOverwrites(deepCopyCommonConfig(cli.BaseConfig.CommonConfig), jobsConfig)
}

func deepCopyBaseConfig(baseConfig BaseConfig) BaseConfig {
	bs, _ := yaml.Marshal(baseConfig)
	newBaseConfig := &BaseConfig{}
	if err := yaml.Unmarshal(bs, newBaseConfig); err != nil {
		exit(err, "failed to unmarshal BaseConfig")
	}
	return *newBaseConfig
}

func deepCopyCommonConfig(commonConfig CommonConfig) CommonConfig {
	bs, _ := yaml.Marshal(commonConfig)
	newCommonConfig := &CommonConfig{}
	if err := yaml.Unmarshal(bs, newCommonConfig); err != nil {
		exit(err, "failed to unmarshal CommonConfig")
	}
	return *newCommonConfig
}

func deepCopyMap(mp map[string]string) map[string]string {
	bs, _ := yaml.Marshal(mp)
	newMap := map[string]string{}
	if err := yaml.Unmarshal(bs, &newMap); err != nil {
		exit(err, "failed to unmarshal Map")
	}
	return newMap
}

func mergeCommonConfig(configs ...CommonConfig) CommonConfig {
	mergedCommonConfig := CommonConfig{}
	for i := 0; i < len(configs); i++ {
		config := deepCopyCommonConfig(configs[i])
		if err := mergo.Merge(&mergedCommonConfig, config,
			mergo.WithAppendSlice, mergo.WithSliceDeepCopy); err != nil {
			exit(err, "failed to merge config")
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

func resolveOverwrites(baseCommonConfig CommonConfig, jobsConfig JobsConfig) JobsConfig {
	jobsConfig.CommonConfig = mergeCommonConfig(baseCommonConfig, jobsConfig.CommonConfig)

	for i, job := range jobsConfig.Jobs {
		job.CommonConfig = mergeCommonConfig(jobsConfig.CommonConfig, job.CommonConfig)

		jobsConfig.Jobs[i] = job
	}

	return jobsConfig
}

// Writes the job yaml
func WriteJobConfig(jobsConfig *JobsConfig, file string) error {
	bytes, err := yaml.Marshal(jobsConfig)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(file, bytes, 0644)
}

func (cli *Client) ValidateJobConfig(fileName string, jobsConfig *JobsConfig) {
	var err error
	if jobsConfig.Org == "" {
		err = multierror.Append(err, fmt.Errorf("%s: org must be set", fileName))
	}
	if jobsConfig.Repo == "" {
		err = multierror.Append(err, fmt.Errorf("%s: repo must be set", fileName))
	}

	for _, job := range jobsConfig.Jobs {
		if job.Image == "" {
			err = multierror.Append(err, fmt.Errorf("%s: image must be set for job %v", fileName, job.Name))
		}
		if job.Resources != "" {
			if _, f := jobsConfig.ResourcePresets[job.Resources]; !f {
				err = multierror.Append(err, fmt.Errorf("%s: job '%v' has nonexistant resource '%v'", fileName, job.Name, job.Resources))
			}
		}
		for _, mod := range job.Modifiers {
			if e := validate(mod, []string{ModifierHidden, ModifierPresubmitOptional, ModifierPresubmitSkipped}, "status"); e != nil {
				err = multierror.Append(err, e)
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
			if e := validate(t, []string{TypePostsubmit, TypePresubmit, TypePeriodic}, "type"); e != nil {
				err = multierror.Append(err, e)
			}
		}
		for _, repo := range job.Repos {
			if len(strings.Split(repo, "/")) != 2 {
				err = multierror.Append(err, fmt.Errorf("%s: repo %v not valid, should take form org/repo", fileName, repo))
			}
		}
	}
	if err != nil {
		exit(err, "validation failed")
	}
}

func (cli *Client) ConvertJobConfig(jobsConfig *JobsConfig, branch string) (config.JobConfig, error) {
	baseConfig := cli.BaseConfig
	testgridConfig := baseConfig.TestgridConfig

	var presubmits []config.Presubmit
	var postsubmits []config.Postsubmit
	var periodics []config.Periodic

	output := config.JobConfig{
		PresubmitsStatic:  map[string][]config.Presubmit{},
		PostsubmitsStatic: map[string][]config.Postsubmit{},
		Periodics:         []config.Periodic{},
	}
	for _, parentJob := range jobsConfig.Jobs {
		expandedJobs := applyMatrixJob(parentJob, jobsConfig.Matrix)
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
					return config.JobConfig{}, err
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
				if job.Trigger != "" {
					defaultTrigger := config.DefaultTriggerFor(presubmit.JobBase.Name)
					// Match the default trigger + the new trigger.
					presubmit.Trigger = fmt.Sprintf("(%s)|((?m)^%s(\\s+|$))", defaultTrigger, job.Trigger)
				}
				if testgridConfig.Enabled {
					presubmit.JobBase.Annotations = mergeMaps(presubmit.JobBase.Annotations, map[string]string{
						TestGridDashboard: testgridJobPrefix,
					})
				}
				applyModifiersPresubmit(&presubmit, job.Modifiers)
				applyRequirements(&presubmit.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
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
					return config.JobConfig{}, err
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
					postsubmit.JobBase.Annotations = mergeMaps(postsubmit.JobBase.Annotations, map[string]string{
						TestGridDashboard:   testgridJobPrefix + "_postsubmit",
						TestGridAlertEmail:  testgridConfig.AlertEmail,
						TestGridNumFailures: testgridConfig.NumFailuresToAlert,
					})
				}
				applyModifiersPostsubmit(&postsubmit, job.Modifiers)
				applyRequirements(&postsubmit.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
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
					return config.JobConfig{}, err
				}
				periodic := config.Periodic{
					JobBase:  base,
					Interval: job.Interval,
					Cron:     job.Cron,
					Tags:     job.Tags,
				}
				if testgridConfig.Enabled {
					periodic.JobBase.Annotations = mergeMaps(periodic.JobBase.Annotations, map[string]string{
						TestGridDashboard:   testgridJobPrefix + "_periodic",
						TestGridAlertEmail:  testgridConfig.AlertEmail,
						TestGridNumFailures: testgridConfig.NumFailuresToAlert,
					})
				}
				applyRequirements(&periodic.JobBase, job.Requirements, job.ExcludedRequirements, jobsConfig.RequirementPresets)
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

func (cli *Client) CheckConfig(jobs config.JobConfig, currentConfigFile string, header string) error {
	current, err := ioutil.ReadFile(currentConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read current config for %s: %v", currentConfigFile, err)
	}

	newConfig, err := yaml.Marshal(jobs)
	if err != nil {
		return fmt.Errorf("failed to marshal result: %v", err)
	}
	if header == "" {
		header = DefaultAutogenHeader
	}
	output := []byte(header + "\n")
	output = append(output, newConfig...)

	if diff := cmp.Diff(output, current); diff != "" {
		return fmt.Errorf("generated config is different from file %s\nWant(-), got(+):\n%s", currentConfigFile, diff)
	}
	return nil
}

func Write(jobs config.JobConfig, fname, header string) {
	bs, err := yaml.Marshal(jobs)
	if err != nil {
		exit(err, "failed to marshal result")
	}
	dir := filepath.Dir(fname)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		exit(err, "failed to create directory: "+dir)
	}
	if header == "" {
		header = DefaultAutogenHeader
	}
	output := []byte(header + "\n")
	output = append(output, bs...)
	err = ioutil.WriteFile(fname, output, 0644)
	if err != nil {
		exit(err, "failed to write result")
	}
}

func Print(c interface{}) {
	bs, err := yaml.Marshal(c)
	if err != nil {
		exit(err, "failed to write result")
	}
	fmt.Println(string(bs))
}

func validate(input string, options []string, description string) error {
	valid := false
	for _, opt := range options {
		if input == opt {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("'%v' is not a valid %v. Must be one of %v", input, description, strings.Join(options, ", "))
	}
	return nil
}

func Diff(result config.JobConfig, existing config.JobConfig) {
	fmt.Println("Presubmit diff:")
	diffConfigPresubmit(result, existing)
	fmt.Println("\n\nPostsubmit diff:")
	diffConfigPostsubmit(result, existing)
}

// FilterReleaseBranchingJobs filters then returns jobs with release branching enabled.
func FilterReleaseBranchingJobs(jobs []Job) []Job {
	jobsF := make([]Job, 0)
	for _, j := range jobs {
		if j.DisableReleaseBranching {
			continue
		}
		jobsF = append(jobsF, j)
	}
	return jobsF
}

func getPresubmit(c config.JobConfig, jobName string) *config.Presubmit {
	presubmits := c.PresubmitsStatic
	for _, jobs := range presubmits {
		for _, job := range jobs {
			if job.Name == jobName {
				return &job
			}
		}
	}
	return nil
}

func diffConfigPresubmit(result config.JobConfig, pj config.JobConfig) {
	known := make(map[string]struct{})
	for _, jobs := range result.PresubmitsStatic {
		for _, job := range jobs {
			known[job.Name] = struct{}{}
			current := getPresubmit(pj, job.Name)
			if current == nil {
				fmt.Println("\nCreated unknown presubmit job", job.Name)
				continue
			}
			diff := pretty.Diff(current, &job)
			if len(diff) > 0 {
				fmt.Println("\nDiff for", job.Name)
			}
			for _, d := range diff {
				fmt.Println(d)
			}
		}
	}
	for _, jobs := range pj.PresubmitsStatic {
		for _, job := range jobs {
			if _, f := known[job.Name]; !f {
				fmt.Println("Missing", job.Name)
			}
		}
	}
}

func diffConfigPostsubmit(result config.JobConfig, pj config.JobConfig) {
	known := make(map[string]struct{})
	var allCurrentPostsubmits []config.Postsubmit
	for _, jobs := range pj.PostsubmitsStatic {
		allCurrentPostsubmits = append(allCurrentPostsubmits, jobs...)
	}
	for _, jobs := range result.PostsubmitsStatic {
		for _, job := range jobs {
			known[job.Name] = struct{}{}
			var current *config.Postsubmit
			for _, ps := range allCurrentPostsubmits {
				ps := ps
				if ps.Name == job.Name {
					current = &ps
					break
				}
			}
			if current == nil {
				fmt.Println("\nCreated unknown job:", job.Name)
				continue

			}
			diff := pretty.Diff(current, &job)
			if len(diff) > 0 {
				fmt.Println("\nDiff for", job.Name)
			}
			for _, d := range diff {
				fmt.Println(d)
			}
		}
	}

	for _, job := range allCurrentPostsubmits {
		if _, f := known[job.Name]; !f {
			fmt.Println("Missing", job.Name)
		}
	}
}

func createContainer(jobConfig *JobsConfig, job *Job, resources map[string]v1.ResourceRequirements) []v1.Container {
	envs := job.Env
	if len(envs) == 0 {
		envs = jobConfig.Env
	} else {
		// TODO: overwrite the env with the same name
		envs = append(envs, jobConfig.Env...)
	}

	c := v1.Container{
		Image:           job.Image,
		SecurityContext: &v1.SecurityContext{Privileged: newTrue()},
		Command:         job.Command,
		Args:            job.Args,
		Env:             envs,
	}
	if job.ImagePullPolicy != "" {
		c.ImagePullPolicy = v1.PullPolicy(job.ImagePullPolicy)
	}
	jobResource := DefaultResource
	if job.Resources != "" {
		jobResource = job.Resources
	}
	if _, ok := resources[jobResource]; ok {
		c.Resources = resources[jobResource]
	}

	return []v1.Container{c}
}

func (cli *Client) createJobBase(baseConfig BaseConfig, jobConfig *JobsConfig, job *Job,
	name string, branch string, resources map[string]v1.ResourceRequirements) (config.JobBase, error) {
	yes := true

	if len(name) > maxJobNameLength && !cli.LongJobNamesAllowed {
		return config.JobBase{}, fmt.Errorf("job name exceeds %v character limit '%v'", maxJobNameLength, name)
	}
	jb := config.JobBase{
		Name:           name,
		MaxConcurrency: job.MaxConcurrency,
		Spec: &v1.PodSpec{
			Containers:   createContainer(jobConfig, job, resources),
			NodeSelector: job.NodeSelector,
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

func applyRequirements(job *config.JobBase, requirements, blockedRequirements []string, presetMap map[string]RequirementPreset) {
	validRequirements := make([]string, 0)
	for name := range presetMap {
		validRequirements = append(validRequirements, name)
	}
	var err error
	for _, req := range requirements {
		if e := validate(
			req,
			validRequirements,
			"requirements"); e != nil {
			err = multierror.Append(err, e)
		}
	}
	for _, req := range blockedRequirements {
		if e := validate(
			req,
			validRequirements,
			"blocked_requirements"); e != nil {
			err = multierror.Append(err, e)
		}
	}
	if err != nil {
		exit(err, "validation failed")
	}

	blocked := sets.NewString(blockedRequirements...)
	presets := make([]RequirementPreset, 0)
	for _, req := range requirements {
		if !blocked.Has(req) {
			presets = append(presets, presetMap[req])
		}
	}
	resolveRequirements(job.Annotations, job.Labels, job.Spec, presets)
}

func applyModifiersPresubmit(presubmit *config.Presubmit, jobModifiers []string) {
	for _, modifier := range jobModifiers {
		if modifier == ModifierPresubmitOptional {
			presubmit.Optional = true
		} else if modifier == ModifierHidden {
			presubmit.SkipReport = true
			presubmit.ReporterConfig = &prowjob.ReporterConfig{
				Slack: &prowjob.SlackReporterConfig{
					JobStatesToReport: []prowjob.ProwJobState{},
				},
			}
		} else if modifier == ModifierPresubmitSkipped {
			presubmit.AlwaysRun = false
		}
	}
}

func applyModifiersPostsubmit(postsubmit *config.Postsubmit, jobModifiers []string) {
	for _, modifier := range jobModifiers {
		if modifier == ModifierPresubmitOptional {
			// Does not exist on postsubmit
		} else if modifier == ModifierHidden {
			postsubmit.SkipReport = true
			f := false
			postsubmit.ReporterConfig = &prowjob.ReporterConfig{
				Slack: &prowjob.SlackReporterConfig{
					Report: &f,
				},
			}
		}
		// Cannot skip a postsubmit; instead just make `type: presubmit`
	}
}

func applyMatrixJob(job Job, matrix map[string][]string) []*Job {
	yamlStr, err := yaml.Marshal(job)
	if err != nil {
		exit(err, "failed to marshal the given Job")
	}
	expandedYamlStr := applyMatrix(string(yamlStr), matrix)
	jobs := make([]*Job, 0)
	for _, jobYaml := range expandedYamlStr {
		job := &Job{}
		if err := yaml.Unmarshal([]byte(jobYaml), job); err != nil {
			exit(err, "failed to unmarshal the yaml to Job")
		}
		jobs = append(jobs, job)
	}
	return jobs
}

func applyMatrix(yamlStr string, matrix map[string][]string) []string {
	subsExps := getVarSubstitutionExpressions(yamlStr)
	if len(subsExps) == 0 {
		return []string{yamlStr}
	}

	combs := make([]string, 0)
	for _, exp := range subsExps {
		if strings.HasPrefix(exp, "matrix.") {
			exp = strings.TrimPrefix(exp, "matrix.")
			if _, ok := matrix[exp]; ok {
				combs = append(combs, exp)
			} else {
				exit(errors.New("dimension is not configured in the matrix"), exp)
			}
		}
	}

	res := &[]string{}
	resolveCombinations(combs, yamlStr, 0, matrix, res)
	return *res
}

func resolveCombinations(combs []string, dest string, start int, matrix map[string][]string, res *[]string) {
	if start == len(combs) {
		*res = append(*res, dest)
		return
	}

	lst := matrix[combs[start]]
	for i := range lst {
		dest := replace(dest, combs[start], lst[i])
		resolveCombinations(combs, dest, start+1, matrix, res)
	}
}

func replace(str, expKey, expVal string) string {
	return strings.ReplaceAll(str, "$(matrix."+expKey+")", expVal)
}

// getVarSubstitutionExpressions extracts all the value between "$(" and ")""
func getVarSubstitutionExpressions(yamlStr string) []string {
	allExpressions := validateString(yamlStr)
	return allExpressions
}

func validateString(value string) []string {
	expressions := variableSubstitutionRegex.FindAllString(value, -1)
	if expressions == nil {
		return nil
	}
	var result []string
	set := map[string]bool{}
	for _, expression := range expressions {
		expression = stripVarSubExpression(expression)
		if _, ok := set[expression]; !ok {
			result = append(result, expression)
			set[expression] = true
		}
	}
	return result
}

func stripVarSubExpression(expression string) string {
	return strings.TrimSuffix(strings.TrimPrefix(expression, "$("), ")")
}

// Reads the generate job config for comparison
func ReadProwJobConfig(file string) config.JobConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		exit(err, "failed to read "+file)
	}
	jobs := config.JobConfig{}
	if err := yaml.Unmarshal(yamlFile, &jobs); err != nil {
		exit(err, "failed to unmarshal "+file)
	}
	return jobs
}

// kubernetes API requires a pointer to a bool for some reason
func newTrue() *bool {
	b := true
	return &b
}

// mergeMaps will merge multiple maps into one.
// If there are duplicated keys in the maps, the value in the later maps will overwrite that of the previous ones.
func mergeMaps(mps ...map[string]string) map[string]string {
	newMap := make(map[string]string)
	for _, mp := range mps {
		for k, v := range mp {
			newMap[k] = v
		}
	}
	return newMap
}
