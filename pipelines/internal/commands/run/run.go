// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package run provides a sub tool for running pipelines.
package run

// This tool runs pipelines using the Google Genomics Pipelines API.
//
// The tool can execute either a single command line (via the --command flag)
// or read and execute an input file consisting of:
// - a raw JSON encoded API request
// - a JSON encoded array of action objects
// - a script file (whose format is described below)
//
// The input filename must be specified as a single positional argument (though
// it can appear before or after other options).  If the input filename is '-',
// the tool reads from standard input.
//
// If a raw request is given as an input the tool does not do any of the
// additional processing described below.
//
// The script file format consists of a series of command lines.  Each line is
// executed as a 'bash' command line in a separate container and must succeed
// before the next command is executed.  This behaviour can be modified using a
// '&' at the end of the line which will cause the command to run in the
// background.  Additionally, flags and options can be specified after a '#'
// character to control what image is used or to apply other action flags.  A
// trailing '\' can be used to break up a long line.
//
// The container image used to execute the command can be changed using the
// --image flag or on a per command basis using "# image=...".  The image must
// contain a 'bash' binary.
//
// Files from GCS can be specified as inputs to the pipeline using the --inputs
// flag.  These files will be copied onto the VM.  The names of the localized
// files are exposed via the environment variables $INPUT0 to $INPUTN, or, if
// the parameters have the form NAME=..., the $NAME environment variable.
//
// In addition to GCS paths, small local files may also be specified as an
// input.  The files will be packaged as part of the request so there are
// significant limitations on the size of the file.  This functionality should
// only be used for inputs such as configuration files or short scripts.
//
// GCS destinations may be specified with the --outputs flag.  Each output file
// will be exposed by via the environment variables $OUTPUT0 to $OUTPUTN.
//
// Entire directories or even subtrees can be localized or delocalized by
// appending the suffixes '/* or '/**' respectively.  The $OUTPUTN variable
// will no longer point to a file, but to a directory where files are either
// copied to (for --inputs) or from (for --outputs).
//
// Since each command is executed in a separate container, disk writes are not
// typically visible between containers.  To facilitate the sharing of files
// between commands, the $TMPDIR variable is set to a writeable path on the
// attached disk.  Files written to this location are not automatically
// delocalized.
//
// As a convenience, the tool will automatically use the cloud SDK image
// whenever the command line starts with gsutil or gcloud, and will
// automatically include the cloud-platform API scope whenever the cloud SDK
// container is used.
//
// If the --output flag is specified, an action is appended that copies the
// combined pipeline output to the specified GCS path.
//
// The --dry-run flag can be used to see what pipeline would be produced
// without executing it.
//
// Example: Simple 'hello world' script
//
//    echo "Hello World!"
//
// Example: Simple 'hello world' actions file
//
//    [
//      {
//        "imageUri": "bash",
//        "commands": [
//          "-c", "echo \"Hello World!\""
//        ]
//      }
//    ]
//
// Example: Background action script
//
//    while true; do echo "Hello background world!"; sleep 1; done &
//    sleep 5
//    echo "Hello foreground world!"
//
// Example: Script to SHA1 sum a file (using --inputs and --outputs).
//
//    sha1sum ${INPUT0} > ${OUTPUT0}
//
// Example: Using netcat to dump logs for the running pipeline
//
//    nc -n -l -p 1234 -e tail -f /google/logs/output & # ports=1234:22
//
import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/googlegenomics/pipelines-tools/pipelines/internal/commands/watch"
	"github.com/googlegenomics/pipelines-tools/pipelines/internal/common"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	genomics "google.golang.org/api/genomics/v2alpha1"
	"google.golang.org/api/googleapi"
)

var (
	googleRoot  = &genomics.Mount{Disk: "google", Path: "/mnt/google"}
	environment = make(map[string]string)
	labels      = make(map[string]string)
	vmLabels    = make(map[string]string)

	flags = flag.NewFlagSet("", flag.ExitOnError)

	basePath       = flags.String("base-path", "", "optional API service base path")
	name           = flags.String("name", "", "optional name applied as a label")
	scopes         = flags.String("scopes", "", "comma separated list of additional API scopes")
	zones          = flags.String("zones", "", "comma separated list of zone names or prefixes (e.g. us-*)")
	regions        = flags.String("regions", "", "comma separated list of region names or prefixes (e.g. us-*)")
	output         = flags.String("output", "", "GCS path to write output to")
	dryRun         = flags.Bool("dry-run", false, "don't run, just show pipeline")
	wait           = flags.Bool("wait", true, "wait for the pipeline to finish")
	machineType    = flags.String("machine-type", "n1-standard-1", "machine type to create")
	inputs         = flags.String("inputs", "", "comma separated list of GCS objects to localize to the VM")
	outputs        = flags.String("outputs", "", "comma separated list of GCS objects to delocalize from the VM")
	diskSizeGb     = flags.Int("disk-size", 0, "if non-zero, overrides the default attached disk size (in GB)")
	diskType       = flags.String("disk-type", "", "the disk type to use for the attached disk(s)")
	diskImage      = flags.String("disk-image", "", "optional image to pre-load onto the attached disk")
	bootDiskSizeGb = flags.Int("boot-disk-size", 0, "if non-zero, specifies the boot disk size (in GB)")
	privateAddress = flags.Bool("private-address", false, "use a private IP address")
	cloudSDKImage  = flags.String("cloud-sdk-image", "gcr.io/cloud-genomics-pipelines/io", "the cloud SDK image to use")
	timeout        = flags.Duration("timeout", 0, "how long to wait before the operation is abandoned")
	defaultImage   = flags.String("image", "bash", "the default image to use when executing commands")
	attempts       = flags.Uint("attempts", 0, "number of attempts on non-fatal failure, using non-preemptible VM")
	pvmAttempts    = flags.Uint("pvm-attempts", 1, "number of attempts on non-fatal failure, using preemptible VM")
	gpus           = flags.Int("gpus", 0, "the number of GPUs to attach")
	gpuType        = flags.String("gpu-type", "nvidia-tesla-k80", "the GPU type to attach")
	command        = flags.String("command", "", "a single command line to execute")
	fuse           = flags.Bool("fuse", false, "if true, use FUSE to localize inputs (see README)")
	ssh            = flags.Bool("ssh", false, "if true, an ssh server will be started")
	network        = flags.String("network", "", "the VPC network to use")
	subnetwork     = flags.String("subnetwork", "", "the VPC subnetwork to use")
	sharePIDs      = flags.Bool("share-pids", false, "if true, all actions will share the same PID namespace")
	cosChannel     = flags.String("cos-channel", "", "if set, specifies the COS release channel to use")
	serviceAccount = flags.String("service-account", "", "if set, specifies the service account for the VM")
	outputInterval = flags.Duration("output-interval", 0, "if non-zero, specifies the time interval for logging output during runs")
)

func init() {
	flags.Var(&common.MapFlagValue{environment}, "set", "sets an environment variable (e.g. NAME[=VALUE])")
	flags.Var(&common.MapFlagValue{labels}, "labels", "label names and values to apply to the operation")
	flags.Var(&common.MapFlagValue{vmLabels}, "vm-labels", "label names and values to apply to the virtual machine")
}

func Invoke(ctx context.Context, service *genomics.Service, project string, arguments []string) error {
	filenames := common.ParseFlags(flags, arguments)

	var filename string
	if len(filenames) > 0 {
		if len(filenames) > 1 {
			return errors.New("only a single input file may be specified")
		}
		filename = filenames[0]
	}

	req, err := buildRequest(filename, project)
	if err != nil {
		return fmt.Errorf("building request: %v", err)
	}

	encoded, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding request: %v", err)
	}
	fmt.Printf("%s\n", encoded)

	if *dryRun || (*attempts == 0 && *pvmAttempts == 0) {
		return nil
	}

	return runPipeline(ctx, service, req)
}

func runPipeline(ctx context.Context, service *genomics.Service, req *genomics.RunPipelineRequest) error {
	abort := make(chan os.Signal, 1)
	signal.Notify(abort, os.Interrupt)

	attempt := uint(1)
	for {
		req.Pipeline.Resources.VirtualMachine.Preemptible = (attempt <= *pvmAttempts)

		lro, err := service.Pipelines.Run(req).Context(ctx).Do()
		if err != nil {
			if err, ok := err.(*googleapi.Error); ok && err.Message != "" {
				return fmt.Errorf("starting pipeline: %q: %q", err.Message, err.Body)
			}
			return fmt.Errorf("starting pipeline: %v", err)
		}

		cancelOnInterrupt(ctx, service, lro.Name, abort)

		fmt.Printf("Pipeline running as %q (attempt: %d, preemptible: %t)\n", lro.Name, attempt, req.Pipeline.Resources.VirtualMachine.Preemptible)
		if *output != "" {
			fmt.Printf("Output will be written to %q\n", *output)
		}

		if !*wait {
			return nil
		}

		if err := watch.Invoke(ctx, service, req.Pipeline.Resources.ProjectId, []string{lro.Name}); err != nil {
			if err, ok := err.(common.PipelineExecutionError); ok && err.IsRetriable() {
				if attempt < *pvmAttempts+*attempts {
					attempt++
					fmt.Printf("Execution failed: %v\n", err)
					continue
				}
			}
			return fmt.Errorf("operation %q failed: %v", lro.Name, err)
		}
		return nil
	}
}

func parseJSON(filename string, v interface{}) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("opening file: %v", err)
	}
	defer f.Close()

	return json.NewDecoder(f).Decode(v)
}

func buildRequest(filename, project string) (*genomics.RunPipelineRequest, error) {
	if filename != "" {
		var req genomics.RunPipelineRequest
		if err := parseJSON(filename, &req); err == nil {
			return &req, nil
		}
	}

	googlePath := func(directory string) string {
		return path.Join(googleRoot.Path, ".google", directory)
	}

	inputRoot := googlePath("input")
	outputRoot := googlePath("output")

	directories := []string{googlePath("tmp")}
	environment["TMPDIR"] = directories[0]

	buckets := make(map[string]string)

	var localizers []*genomics.Action
	for input, name := range namedListOf(*inputs, "INPUT") {
		filename := gcsJoin(inputRoot, strings.TrimRight(input, "*"))
		environment[name] = filename
		if bucket, ok := parseGCSPath(input); ok {
			if *fuse {
				buckets[bucket] = filepath.Join(inputRoot, bucket)
				continue
			}
			localizers = append(localizers, gcsTransfer(input)(input, filename))
		} else {
			action, err := upload(input, filename)
			if err != nil {
				return nil, fmt.Errorf("processing %q: %v", input, err)
			}
			localizers = append(localizers, action)
		}
		if strings.HasSuffix(input, "*") {
			directories = append(directories, filename)
		}
	}

	if len(buckets) > 0 {
		for _, path := range buckets {
			directories = append(directories, path)
		}
		localizers = append(localizers, gcsFuse(buckets)...)
	}

	var delocalizers []*genomics.Action
	for output, name := range namedListOf(*outputs, "OUTPUT") {
		filename := gcsJoin(outputRoot, strings.TrimRight(output, "*"))
		delocalizers = append(delocalizers, gcsTransfer(output)(filename, output))
		environment[name] = filename
		if strings.HasSuffix(output, "*") {
			directories = append(directories, filename)
		} else {
			directories = append(directories, path.Dir(filename))
		}
	}

	if *output != "" {
		action := gsutil("cp", "/google/logs/output", *output)
		action.Flags = []string{"ALWAYS_RUN"}
		delocalizers = append(delocalizers, action)
	}

	var actions []*genomics.Action
	if *outputInterval != 0 && *output != "" {
		action := bash(fmt.Sprintf("while true; do sleep %.0f; gsutil -q cp /google/logs/output %s; done", (*outputInterval).Seconds(), *output))
		action.Flags = []string{"RUN_IN_BACKGROUND"}
		actions = append(actions, action)
	}

	if filename != "" {
		v, err := parseFile(filename)
		if err != nil {
			return nil, fmt.Errorf("creating pipeline from file: %v", err)
		}
		actions = append(actions, v...)
	} else if *command != "" {
		action, err := parse(*command)
		if err != nil {
			return nil, fmt.Errorf("creating action from command: %v", err)
		}
		actions = append(actions, action)
	} else {
		return nil, errors.New("no command or input file was specified")
	}

	vm := &genomics.VirtualMachine{
		MachineType: *machineType,
		Network: &genomics.Network{
			UsePrivateAddress: *privateAddress,
		},
		ServiceAccount: &genomics.ServiceAccount{
			Email:  *serviceAccount,
			Scopes: listOf(*scopes),
		},
		Labels: vmLabels,
	}

	if channel := *cosChannel; channel != "" {
		vm.BootImage = "projects/cos-cloud/global/images/family/cos-" + channel
	}

	if *network != "" {
		vm.Network.Name = *network
	}
	if *subnetwork != "" {
		vm.Network.Subnetwork = *subnetwork
	}

	if *gpus > 0 {
		vm.Accelerators = append(vm.Accelerators, &genomics.Accelerator{
			Type:  *gpuType,
			Count: int64(*gpus),
		})
	}

	resources := &genomics.Resources{
		ProjectId:      project,
		VirtualMachine: vm,
	}
	if *regions != "" && *zones != "" {
		return nil, errors.New("both zones and regions have been supplied")
	}
	if *regions != "" {
		regions, err := expandPrefixes(project, listOf(*regions), listRegions)
		if err != nil {
			return nil, fmt.Errorf("expanding regions: %v", err)
		}
		resources.Regions = regions
    }
    if *zones != "" {
		zones, err := expandPrefixes(project, listOf(*zones), listZones)
		if err != nil {
			return nil, fmt.Errorf("expanding zones: %v", err)
		}
		resources.Zones = zones
    }
    if len(resources.Zones) + len(resources.Regions) == 0 {
        resources.Zones = []string{"us-east1-d"}
    }

	pipeline := &genomics.Pipeline{
		Resources:   resources,
		Environment: environment,
	}

	pipeline.Actions = []*genomics.Action{mkdir(directories)}

	if *ssh {
		pipeline.Actions = append(pipeline.Actions, sshDebug(project))
	}

	for _, v := range [][]*genomics.Action{localizers, actions, delocalizers} {
		pipeline.Actions = append(pipeline.Actions, v...)
	}

	addRequiredDisks(pipeline)
	addRequiredScopes(pipeline)

	if *sharePIDs {
		for _, action := range pipeline.Actions {
			action.PidNamespace = "shared"
		}
	}

	if *name != "" {
		labels["name"] = *name
	}

	if *timeout != 0 {
		pipeline.Timeout = fmt.Sprintf("%.0fs", (*timeout).Seconds())
	}

	return &genomics.RunPipelineRequest{Pipeline: pipeline, Labels: labels}, nil
}

func parseFile(filename string) ([]*genomics.Action, error) {
	var scanner *bufio.Scanner
	if filename == "-" {
		scanner = bufio.NewScanner(os.Stdin)
	} else {
		var actions []*genomics.Action
		if parseJSON(filename, &actions) == nil {
			return actions, nil
		}

		f, err := os.Open(filename)
		if err != nil {
			return nil, fmt.Errorf("opening script: %v", err)
		}
		defer f.Close()

		scanner = bufio.NewScanner(f)
	}

	var line int
	var buffer strings.Builder
	var actions []*genomics.Action
	for scanner.Scan() {
		text := scanner.Text()
		line++

		if strings.HasSuffix(text, "\\") {
			buffer.WriteString(text[:len(text)-1])
			continue
		}

		buffer.WriteString(text)

		action, err := parse(buffer.String())
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", line, err)
		}
		if action != nil {
			actions = append(actions, action)
		}

		buffer.Reset()
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading script: %v", err)
	}
	return actions, nil
}

func parse(line string) (*genomics.Action, error) {
	var action genomics.Action

	options := make(map[string]string)
	if n := strings.Index(line, "#"); n >= 0 {
		for _, option := range strings.Fields(strings.TrimSpace(line[n+1:])) {
			if n := strings.Index(option, "="); n >= 0 {
				options[option[:n]] = option[n+1:]
			} else {
				action.Flags = append(action.Flags, strings.ToUpper(option))
			}
		}
		line = line[:n]
	}

	commands := strings.Fields(strings.TrimSpace(line))
	if len(commands) > 0 {
		if commands[len(commands)-1] == "&" {
			action.Flags = append(action.Flags, "RUN_IN_BACKGROUND")
			commands = commands[:len(commands)-1]
		}
		action.Commands = []string{"-c", strings.Join(commands, " ")}
		action.Entrypoint = "bash"
	}

	if timeout, ok := options["timeout"]; ok {
		duration, err := time.ParseDuration(timeout)
		if err != nil {
			return nil, fmt.Errorf("parsing action timeout: %v", err)
		}
		action.Timeout = fmt.Sprintf("%.0fs", duration.Seconds())
	}

	action.ImageUri = detectImage(commands, options)
	action.Mounts = []*genomics.Mount{googleRoot}

	if v, ok := options["ports"]; ok {
		ports, err := parsePorts(v)
		if err != nil {
			return nil, fmt.Errorf("parsing ports: %v", err)
		}
		action.PortMappings = ports
	}
	return &action, nil
}

func detectImage(command []string, options map[string]string) string {
	if image, ok := options["image"]; ok {
		return image
	}
	if len(command) > 0 && isCloudCommand(command[0]) {
		return *cloudSDKImage
	}
	return *defaultImage
}

func addRequiredDisks(pipeline *genomics.Pipeline) {
	disks := make(map[string]bool)
	for _, action := range pipeline.Actions {
		for _, mount := range action.Mounts {
			disks[mount.Disk] = true
		}
	}

	vm := pipeline.Resources.VirtualMachine
	for name := range disks {
		disk := &genomics.Disk{
			Name:   name,
			Type:   *diskType,
			SizeGb: int64(*diskSizeGb),
		}
		if *diskImage != "" && name == googleRoot.Disk {
			disk.SourceImage = *diskImage
		}
		vm.Disks = append(vm.Disks, disk)
	}
	if *bootDiskSizeGb > 0 {
		vm.BootDiskSizeGb = int64(*bootDiskSizeGb)
	}
}

func addRequiredScopes(pipeline *genomics.Pipeline) {
	scopes := &pipeline.Resources.VirtualMachine.ServiceAccount.Scopes
	for _, action := range pipeline.Actions {
		if action.ImageUri == *cloudSDKImage || (len(action.Commands) > 0 && isCloudCommand(action.Commands[0])) {
			*scopes = append(*scopes, "https://www.googleapis.com/auth/devstorage.read_write")
			return
		}
	}
}

func isCloudCommand(command string) bool {
	return command == "gsutil" || command == "gcloud"
}

func listOf(input string) []string {
	if input == "" {
		return nil
	}
	return strings.Split(input, ",")
}

func namedListOf(input, defaultPrefix string) map[string]string {
	output := make(map[string]string)
	for n, input := range strings.Split(input, ",") {
		if i := strings.Index(input, "="); i > 0 {
			output[input[i+1:]] = input[:i]
		} else if input != "" {
			output[input] = fmt.Sprintf("%s%d", defaultPrefix, n)
		}
	}
	return output
}

func expandPrefixes(project string, input []string, allValues func(project string, service *compute.Service) ([]string, error)) ([]string, error) {
	var results, prefixes []string
	for _, item := range input {
		if strings.HasSuffix(item, "*") {
			prefixes = append(prefixes, item[:len(item)-1])
		} else {
			results = append(results, item)
		}
	}
	if len(prefixes) > 0 {
		client, err := google.DefaultClient(context.Background(), compute.ComputeScope)
		if err != nil {
			return nil, fmt.Errorf("creating compute client: %v", err)
		}
		service, err := compute.New(client)
		if err != nil {
			return nil, fmt.Errorf("creating compute service: %v", err)
		}
		values, err := allValues(project, service)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			for _, prefix := range prefixes {
				if strings.HasPrefix(value, prefix) {
					results = append(results, value)
					break
				}
			}
		}
	}
	return results, nil
}

func listZones(project string, service *compute.Service) ([]string, error) {
	resp, err := service.Zones.List(project).Do()
	if err != nil {
		return nil, fmt.Errorf("listing zones: %v", err)
	}
	var zones []string
	for _, zone := range resp.Items {
		zones = append(zones, zone.Name)
	}
	return zones, nil
}

func listRegions(project string, service *compute.Service) ([]string, error) {
	resp, err := service.Regions.List(project).Do()
	if err != nil {
		return nil, fmt.Errorf("listing regions: %v", err)
	}
	var regions []string
	for _, region := range resp.Items {
		regions = append(regions, region.Name)
	}
	return regions, nil
}

func parsePorts(input string) (map[string]int64, error) {
	ports := make(map[string]int64)
	for _, pair := range strings.Split(input, ";") {
		i := strings.Index(pair, ":")
		if i < 1 {
			return nil, fmt.Errorf("invalid port mapping %q", pair)
		}
		n, err := strconv.ParseInt(pair[i+1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parsing host port: %v", err)
		}
		ports[pair[:i]] = n
	}
	return ports, nil
}

func gsutil(arguments ...string) *genomics.Action {
	return bash("gsutil -q " + strings.Join(arguments, " "))
}

func upload(input, output string) (*genomics.Action, error) {
	raw, err := ioutil.ReadFile(input)
	if err != nil {
		return nil, fmt.Errorf("reading input file: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	return bash(
		fmt.Sprintf("mkdir -p %q", path.Dir(output)),
		fmt.Sprintf("echo %q | base64 -d > %q", encoded, output),
	), nil
}

func bash(commands ...string) *genomics.Action {
	return &genomics.Action{
		ImageUri:   *cloudSDKImage,
		Commands:   []string{"-c", strings.Join(commands, " && ")},
		Mounts:     []*genomics.Mount{googleRoot},
		Entrypoint: "bash",
	}
}

func mkdir(directories []string) *genomics.Action {
	if len(directories) == 0 {
		return nil
	}

	sort.Strings(directories)
	arguments := []string{directories[0]}
	for _, directory := range directories[1:] {
		if strings.HasPrefix(directory, arguments[len(arguments)-1]) {
			arguments[len(arguments)-1] = directory
		} else {
			arguments = append(arguments, directory)
		}
	}

	return bash("mkdir -p " + strings.Join(arguments, " "))
}

func cancelOnInterrupt(ctx context.Context, service *genomics.Service, name string, abort chan os.Signal) {
	go func() {
		<-abort
		fmt.Println("Cancelling operation...")
		req := &genomics.CancelOperationRequest{}
		if _, err := service.Projects.Operations.Cancel(name, req).Context(ctx).Do(); err != nil {
			fmt.Printf("Failed to cancel operation: %v\n", err)
		}
	}()
}

const gcsPrefix = "gs://"

func parseGCSPath(input string) (string, bool) {
	parsed, err := url.Parse(input)
	if err != nil {
		return "", false
	}
	return parsed.Host, true
}

func gcsJoin(input ...string) string {
	parts := make([]string, len(input))
	for i, part := range input {
		parts[i] = strings.TrimPrefix(part, gcsPrefix)
	}
	if len(input) > 0 && strings.HasPrefix(input[0], gcsPrefix) {
		return gcsPrefix + path.Join(parts...)
	}
	return path.Join(parts...)
}

func gcsTransfer(remote string) func(from, to string) *genomics.Action {
	return func(from, to string) *genomics.Action {
		from = strings.TrimRight(from, "*")
		to = strings.TrimRight(to, "*")
		if strings.HasSuffix(remote, "/**") {
			return gsutil("-m", "cp", "-r", gcsJoin(from, "*"), to)
		}
		if strings.HasSuffix(remote, "/*") {
			return gsutil("-m", "cp", gcsJoin(from, "*"), to)
		}
		return gsutil("cp", from, to)
	}
}

func gcsFuse(buckets map[string]string) []*genomics.Action {
	var actions []*genomics.Action
	for bucket, path := range buckets {
		actions = append(actions, &genomics.Action{
			ImageUri: "gcr.io/cloud-genomics-pipelines/gcsfuse",
			Commands: []string{"--implicit-dirs", "--foreground", bucket, path},
			Flags:    []string{"ENABLE_FUSE", "RUN_IN_BACKGROUND"},
			Mounts:   []*genomics.Mount{googleRoot},
		})
		actions = append(actions, &genomics.Action{
			ImageUri: "gcr.io/cloud-genomics-pipelines/gcsfuse",
			Commands: []string{"wait", path},
			Mounts:   []*genomics.Mount{googleRoot},
		})
	}
	return actions
}

func sshDebug(project string) *genomics.Action {
	return &genomics.Action{
		ImageUri:     "gcr.io/cloud-genomics-pipelines/tools",
		Entrypoint:   "ssh-server",
		PortMappings: map[string]int64{"22": 22},
		Flags:        []string{"RUN_IN_BACKGROUND"},
	}
}
