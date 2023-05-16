// Package taskhash handles calculating dependency hashes for nodes in the task execution graph.
package taskhash

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/pyr-sh/dag"
	"github.com/vercel/turbo/cli/internal/env"
	"github.com/vercel/turbo/cli/internal/fs"
	"github.com/vercel/turbo/cli/internal/hashing"
	"github.com/vercel/turbo/cli/internal/inference"
	"github.com/vercel/turbo/cli/internal/nodes"
	"github.com/vercel/turbo/cli/internal/runsummary"
	"github.com/vercel/turbo/cli/internal/turbopath"
	"github.com/vercel/turbo/cli/internal/util"
	"github.com/vercel/turbo/cli/internal/workspace"
	"golang.org/x/sync/errgroup"
)

// Tracker caches package-inputs hashes, as well as package-task hashes.
// package-inputs hashes must be calculated before package-task hashes,
// and package-task hashes must be calculated in topographical order.
// package-task hashing is threadsafe, provided topographical order is
// respected.
type Tracker struct {
	rootNode   string
	globalHash string
	pipeline   fs.Pipeline

	packageInputsHashes map[string]string

	// packageInputsExpandedHashes is a map of a hashkey to a list of files that are inputs to the task.
	// Writes to this map happen during CalculateFileHash(). Since this happens synchronously
	// before walking the task graph, it does not need to be protected by a mutex.
	packageInputsExpandedHashes map[string]map[turbopath.AnchoredUnixPath]string

	// mu is a mutex that we can lock/unlock to read/write from maps
	// the fields below should be protected by the mutex.
	mu                     sync.RWMutex
	packageTaskEnvVars     map[string]env.DetailedMap // taskId -> envvar pairs that affect the hash.
	packageTaskHashes      map[string]string          // taskID -> hash
	packageTaskFramework   map[string]string          // taskID -> inferred framework for package
	packageTaskOutputs     map[string][]turbopath.AnchoredSystemPath
	packageTaskCacheStatus map[string]runsummary.TaskCacheSummary
}

// NewTracker creates a tracker for package-inputs combinations and package-task combinations.
func NewTracker(rootNode string, globalHash string, pipeline fs.Pipeline) *Tracker {
	return &Tracker{
		rootNode:               rootNode,
		globalHash:             globalHash,
		pipeline:               pipeline,
		packageTaskHashes:      make(map[string]string),
		packageTaskFramework:   make(map[string]string),
		packageTaskEnvVars:     make(map[string]env.DetailedMap),
		packageTaskOutputs:     make(map[string][]turbopath.AnchoredSystemPath),
		packageTaskCacheStatus: make(map[string]runsummary.TaskCacheSummary),
	}
}

// packageFileHashInputs defines a combination of a package and optional set of input globs
type packageFileHashInputs struct {
	taskID         string
	taskDefinition *fs.TaskDefinition
	packageName    string
}

// CalculateFileHashes hashes each unique package-inputs combination that is present
// in the task graph. Must be called before calculating task hashes.
func (th *Tracker) CalculateFileHashes(
	allTasks []dag.Vertex,
	workerCount int,
	workspaceInfos workspace.Catalog,
	taskDefinitions map[string]*fs.TaskDefinition,
	repoRoot turbopath.AbsoluteSystemPath,
) error {
	hashTasks := make(util.Set)

	for _, v := range allTasks {
		taskID, ok := v.(string)
		if !ok {
			return fmt.Errorf("unknown task %v", taskID)
		}
		if taskID == th.rootNode {
			continue
		}

		packageName, _ := util.GetPackageTaskFromId(taskID)
		if packageName == th.rootNode {
			continue
		}

		taskDefinition, ok := taskDefinitions[taskID]
		if !ok {
			return fmt.Errorf("missing pipeline entry %v", taskID)
		}

		pfs := &packageFileHashInputs{
			taskID,
			taskDefinition,
			packageName,
		}

		hashTasks.Add(pfs)
	}

	hashes := make(map[string]string, len(hashTasks))
	hashObjects := make(map[string]map[turbopath.AnchoredUnixPath]string, len(hashTasks))
	hashQueue := make(chan *packageFileHashInputs, workerCount)
	hashErrs := &errgroup.Group{}

	for i := 0; i < workerCount; i++ {
		hashErrs.Go(func() error {
			for packageFileHashInputs := range hashQueue {
				pkg, ok := workspaceInfos.PackageJSONs[packageFileHashInputs.packageName]
				if !ok {
					return fmt.Errorf("cannot find package %v", packageFileHashInputs.packageName)
				}

				// Get the hashes of each file, keyed by the path.
				hashObject, err := hashing.GetPackageFileHashes(repoRoot, pkg.Dir, packageFileHashInputs.taskDefinition.Inputs)
				if err != nil {
					return err
				}

				// Get the combined hash of all the files.
				hash, err := fs.HashObject(hashObject)
				if err != nil {
					return err
				}

				// Save off the hash information, keyed by package task.
				th.mu.Lock()
				hashes[packageFileHashInputs.taskID] = hash
				hashObjects[packageFileHashInputs.taskID] = hashObject
				th.mu.Unlock()
			}
			return nil
		})
	}
	for ht := range hashTasks {
		hashQueue <- ht.(*packageFileHashInputs)
	}
	close(hashQueue)
	err := hashErrs.Wait()
	if err != nil {
		return err
	}
	th.packageInputsHashes = hashes
	th.packageInputsExpandedHashes = hashObjects
	return nil
}

type taskHashable struct {
	packageDir           turbopath.AnchoredUnixPath
	hashOfFiles          string
	externalDepsHash     string
	task                 string
	outputs              fs.TaskOutputs
	passThruArgs         []string
	envMode              util.EnvMode
	passthroughEnv       []string
	hashableEnvPairs     []string
	globalHash           string
	taskDependencyHashes []string
}

type oldTaskHashable struct {
	packageDir           turbopath.AnchoredUnixPath
	hashOfFiles          string
	externalDepsHash     string
	task                 string
	outputs              fs.TaskOutputs
	passThruArgs         []string
	hashableEnvPairs     []string
	globalHash           string
	taskDependencyHashes []string
}

// calculateTaskHashFromHashable returns a hash string from the taskHashable
func calculateTaskHashFromHashable(full *taskHashable, useOldTaskHashable bool) (string, error) {
	// The user is not using the strict environment variables feature.
	if useOldTaskHashable {
		return fs.HashObject(&oldTaskHashable{
			packageDir:           full.packageDir,
			hashOfFiles:          full.hashOfFiles,
			externalDepsHash:     full.externalDepsHash,
			task:                 full.task,
			outputs:              full.outputs,
			passThruArgs:         full.passThruArgs,
			hashableEnvPairs:     full.hashableEnvPairs,
			globalHash:           full.globalHash,
			taskDependencyHashes: full.taskDependencyHashes,
		})
	}

	switch full.envMode {
	case util.Loose:
		// Remove the passthroughs from hash consideration if we're explicitly loose.
		full.passthroughEnv = nil
		return fs.HashObject(full)
	case util.Strict:
		// Collapse `nil` and `[]` in strict mode.
		if full.passthroughEnv == nil {
			full.passthroughEnv = make([]string, 0)
		}
		return fs.HashObject(full)
	case util.Infer:
		panic("task inferred status should have already been resolved")
	default:
		panic("unimplemented environment mode")
	}
}

func (th *Tracker) calculateDependencyHashes(dependencySet dag.Set) ([]string, error) {
	dependencyHashSet := make(util.Set)

	rootPrefix := th.rootNode + util.TaskDelimiter
	th.mu.RLock()
	defer th.mu.RUnlock()
	for _, dependency := range dependencySet {
		if dependency == th.rootNode {
			continue
		}
		dependencyTask, ok := dependency.(string)
		if !ok {
			return nil, fmt.Errorf("unknown task: %v", dependency)
		}
		if strings.HasPrefix(dependencyTask, rootPrefix) {
			continue
		}
		dependencyHash, ok := th.packageTaskHashes[dependencyTask]
		if !ok {
			return nil, fmt.Errorf("missing hash for dependent task: %v", dependencyTask)
		}
		dependencyHashSet.Add(dependencyHash)
	}
	dependenciesHashList := dependencyHashSet.UnsafeListOfStrings()
	sort.Strings(dependenciesHashList)
	return dependenciesHashList, nil
}

// CalculateTaskHash calculates the hash for package-task combination. It is threadsafe, provided
// that it has previously been called on its task-graph dependencies. File hashes must be calculated
// first.
func (th *Tracker) CalculateTaskHash(logger hclog.Logger, packageTask *nodes.PackageTask, dependencySet dag.Set, frameworkInference bool, args []string, useOldTaskHashable bool) (string, error) {
	hashOfFiles, ok := th.packageInputsHashes[packageTask.TaskID]
	if !ok {
		return "", fmt.Errorf("cannot find package-file hash for %v", packageTask.TaskID)
	}

	var keyMatchers []string
	var framework *inference.Framework
	envVarContainingExcludePrefix := ""

	if frameworkInference {
		envVarContainingExcludePrefix = "TURBO_CI_VENDOR_ENV_KEY"
		framework = inference.InferFramework(packageTask.Pkg)
		if framework != nil && framework.EnvMatcher != "" {
			// log auto detected framework and env prefix
			logger.Debug(fmt.Sprintf("auto detected framework for %s", packageTask.PackageName), "framework", framework.Slug, "env_prefix", framework.EnvMatcher)
			keyMatchers = append(keyMatchers, framework.EnvMatcher)
		}
	}

	envVars, err := env.GetHashableEnvVars(
		packageTask.TaskDefinition.EnvVarDependencies,
		keyMatchers,
		envVarContainingExcludePrefix,
	)
	if err != nil {
		return "", err
	}
	hashableEnvPairs := envVars.All.ToHashable()
	outputs := packageTask.HashableOutputs()
	taskDependencyHashes, err := th.calculateDependencyHashes(dependencySet)
	if err != nil {
		return "", err
	}
	// log any auto detected env vars
	logger.Debug(fmt.Sprintf("task hash env vars for %s:%s", packageTask.PackageName, packageTask.Task), "vars", hashableEnvPairs)

	hash, err := calculateTaskHashFromHashable(&taskHashable{
		packageDir:           packageTask.Pkg.Dir.ToUnixPath(),
		hashOfFiles:          hashOfFiles,
		externalDepsHash:     packageTask.Pkg.ExternalDepsHash,
		task:                 packageTask.Task,
		outputs:              outputs.Sort(),
		passThruArgs:         args,
		envMode:              packageTask.EnvMode,
		passthroughEnv:       packageTask.TaskDefinition.PassthroughEnv,
		hashableEnvPairs:     hashableEnvPairs,
		globalHash:           th.globalHash,
		taskDependencyHashes: taskDependencyHashes,
	}, useOldTaskHashable)
	if err != nil {
		return "", fmt.Errorf("failed to hash task %v: %v", packageTask.TaskID, hash)
	}
	th.mu.Lock()
	th.packageTaskEnvVars[packageTask.TaskID] = envVars
	th.packageTaskHashes[packageTask.TaskID] = hash
	if framework != nil {
		th.packageTaskFramework[packageTask.TaskID] = framework.Slug
	}
	th.mu.Unlock()
	return hash, nil
}

// GetExpandedInputs gets the expanded set of inputs for a given PackageTask
func (th *Tracker) GetExpandedInputs(packageTask *nodes.PackageTask) map[turbopath.AnchoredUnixPath]string {
	expandedInputs := th.packageInputsExpandedHashes[packageTask.TaskID]
	inputsCopy := make(map[turbopath.AnchoredUnixPath]string, len(expandedInputs))

	for path, hash := range expandedInputs {
		inputsCopy[path] = hash
	}

	return inputsCopy
}

// GetEnvVars returns the hashed env vars for a given taskID
func (th *Tracker) GetEnvVars(taskID string) env.DetailedMap {
	th.mu.RLock()
	defer th.mu.RUnlock()
	return th.packageTaskEnvVars[taskID]
}

// GetFramework returns the inferred framework for a given taskID
func (th *Tracker) GetFramework(taskID string) string {
	th.mu.RLock()
	defer th.mu.RUnlock()
	return th.packageTaskFramework[taskID]
}

// GetExpandedOutputs returns a list of outputs for a given taskID
func (th *Tracker) GetExpandedOutputs(taskID string) []turbopath.AnchoredSystemPath {
	th.mu.RLock()
	defer th.mu.RUnlock()
	outputs, ok := th.packageTaskOutputs[taskID]

	if !ok {
		return []turbopath.AnchoredSystemPath{}
	}

	return outputs
}

// SetExpandedOutputs a list of outputs for a given taskID so it can be read later
func (th *Tracker) SetExpandedOutputs(taskID string, outputs []turbopath.AnchoredSystemPath) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.packageTaskOutputs[taskID] = outputs
}

// SetCacheStatus records the task status for the given taskID
func (th *Tracker) SetCacheStatus(taskID string, cacheSummary runsummary.TaskCacheSummary) {
	th.mu.Lock()
	defer th.mu.Unlock()
	th.packageTaskCacheStatus[taskID] = cacheSummary
}

// GetCacheStatus records the task status for the given taskID
func (th *Tracker) GetCacheStatus(taskID string) runsummary.TaskCacheSummary {
	th.mu.Lock()
	defer th.mu.Unlock()

	if status, ok := th.packageTaskCacheStatus[taskID]; ok {
		return status
	}

	// Return an empty one, all the fields will be false and 0
	return runsummary.TaskCacheSummary{}
}