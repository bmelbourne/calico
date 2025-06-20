// Copyright (c) 2020-2021 Tigera, Inc. All rights reserved.
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

package workload

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/felix/fv/connectivity"
	"github.com/projectcalico/calico/felix/fv/containers"
	"github.com/projectcalico/calico/felix/fv/infrastructure"
	"github.com/projectcalico/calico/felix/fv/tcpdump"
	"github.com/projectcalico/calico/felix/fv/utils"
	api "github.com/projectcalico/calico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/conversion"
	client "github.com/projectcalico/calico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/calico/libcalico-go/lib/options"
)

type Workload struct {
	C                     *containers.Container
	Name                  string
	InterfaceName         string
	IP                    string
	IP6                   string
	Ports                 string
	DefaultPort           string
	runCmd                *exec.Cmd
	outPipe               io.ReadCloser
	errPipe               io.ReadCloser
	namespacePath         string
	WorkloadEndpoint      *api.WorkloadEndpoint
	Protocol              string // "tcp" or "udp"
	SpoofInterfaceName    string
	SpoofName             string
	SpoofWorkloadEndpoint *api.WorkloadEndpoint
	MTU                   int
	isRunning             bool
	isSpoofing            bool
	listenAnyIP           bool
	pid                   string

	cleanupLock sync.Mutex
}

func (w *Workload) GetIP() string {
	return w.IP
}

func (w *Workload) GetInterfaceName() string {
	return w.InterfaceName
}

func (w *Workload) GetSpoofInterfaceName() string {
	if w.isSpoofing {
		return w.SpoofInterfaceName
	}
	return ""
}

func (w *Workload) Runs() bool {
	return w.isRunning
}

var (
	workloadIdx = 0
	sideServIdx = 0
)

const defaultMTU = 1440 /* wiregueard mtu */

func (w *Workload) Stop() {
	if w == nil {
		log.Info("Stop no-op because nil workload")
	} else {
		log.WithField("workload", w.Name).Info("Stop")
		_ = w.C.ExecMayFail("sh", "-c", fmt.Sprintf("kill -9 %s & ip link del %s & ip netns del %s & wait", w.pid, w.InterfaceName, w.NamespaceID()))
		// Killing the process inside the container should cause our long-running
		// docker exec command to exit.  Do the Wait on a background goroutine,
		// so we can time it out, just in case.
		waitDone := make(chan struct{})
		go func() {
			defer close(waitDone)
			_, err := w.runCmd.Process.Wait()
			if err != nil {
				log.WithField("workload", w.Name).Error("Failed to wait for docker exec, attempting to kill it.")
				_ = w.runCmd.Process.Kill()
			}
		}()

		select {
		case <-waitDone:
			log.WithField("workload", w.Name).Info("Workload stopped")
		case <-time.After(10 * time.Second):
			log.WithField("workload", w.Name).Error("Workload docker exec failed to exit?  Killing it.")
			_ = w.runCmd.Process.Kill()
		}

		w.isRunning = false
	}
}

func Run(c *infrastructure.Felix, name, profile, ip, ports, protocol string, opts ...Opt) (w *Workload) {
	w, err := run(c, name, profile, ip, ports, protocol, opts...)
	if err != nil {
		log.WithError(err).Info("Starting workload failed, retrying")
		w, err = run(c, name, profile, ip, ports, protocol, opts...)
	}
	Expect(err).NotTo(HaveOccurred())

	return w
}

type Opt func(*Workload)

func WithMTU(mtu int) Opt {
	return func(w *Workload) {
		w.MTU = mtu
	}
}

func WithIPv6Address(ipv6 string) Opt {
	return func(w *Workload) {
		w.IP6 = ipv6
	}
}

func WithListenAnyIP() Opt {
	return func(w *Workload) {
		w.listenAnyIP = true
	}
}

// WithHostNetworked force the workload to be host-networked even if the listen IP is
// different than the host IP.
func WithHostNetworked() Opt {
	return func(w *Workload) {
		w.InterfaceName = ""
	}
}

func New(c *infrastructure.Felix, name, profile, ip, ports, protocol string, opts ...Opt) *Workload {
	workloadIdx++
	n := fmt.Sprintf("%s-idx%v", name, workloadIdx)
	interfaceName := conversion.NewConverter().VethNameForWorkload(profile, n)
	spoofN := fmt.Sprintf("%s-spoof%v", name, workloadIdx)
	spoofIfaceName := conversion.NewConverter().VethNameForWorkload(profile, spoofN)
	if c.IP == ip || c.IPv6 == ip {
		interfaceName = ""
		spoofIfaceName = ""
	}
	// Build unique workload name and struct.
	workloadIdx++

	wep := api.NewWorkloadEndpoint()
	wep.Labels = map[string]string{"name": n}
	wep.Spec.Node = c.Hostname
	wep.Spec.Orchestrator = "felixfv"
	wep.Spec.Workload = n
	wep.Spec.Endpoint = n
	prefixLen := "32"
	if strings.Contains(ip, ":") {
		prefixLen = "128"
	}
	wep.Spec.IPNetworks = []string{ip + "/" + prefixLen}
	wep.Spec.InterfaceName = interfaceName
	wep.Spec.Profiles = []string{profile}

	workload := &Workload{
		C:                  c.Container,
		Name:               n,
		IP:                 ip,
		SpoofName:          spoofN,
		InterfaceName:      interfaceName,
		SpoofInterfaceName: spoofIfaceName,
		Ports:              ports,
		Protocol:           protocol,
		WorkloadEndpoint:   wep,
		MTU:                defaultMTU,
	}

	for _, o := range opts {
		o(workload)
	}

	if workload.IP6 != "" {
		wep.Spec.IPNetworks = append(wep.Spec.IPNetworks, workload.IP6+"/128")
	}

	c.Workloads = append(c.Workloads, workload)
	return workload
}

func run(c *infrastructure.Felix, name, profile, ip, ports, protocol string, opts ...Opt) (w *Workload, err error) {
	w = New(c, name, profile, ip, ports, protocol, opts...)
	return w, w.Start()
}

func (w *Workload) Start() error {
	var err error

	// Start the workload.
	log.WithField("workload", w).Info("About to run workload")
	var protoArg string
	if w.Protocol != "" {
		protoArg = "--protocol=" + w.Protocol
	}

	wIP := w.IP
	if w.IP6 != "" {
		wIP = wIP + "," + w.IP6
	}
	command := fmt.Sprintf("echo $$; exec test-workload %v '%v' '%v' '%v'",
		protoArg,
		w.InterfaceName,
		wIP,
		w.Ports,
	)

	if w.MTU != 0 {
		command += fmt.Sprintf(" --mtu=%d", w.MTU)
	}

	if w.listenAnyIP {
		command += " --listen-any-ip"
	}

	w.runCmd = utils.Command("docker", "exec", w.C.Name, "sh", "-c", command)
	w.outPipe, err = w.runCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("Getting StdoutPipe failed: %v", err)
	}
	w.errPipe, err = w.runCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("Getting StderrPipe failed: %v", err)
	}
	err = w.runCmd.Start()
	if err != nil {
		return fmt.Errorf("runCmd Start failed: %v", err)
	}

	// Read the workload's namespace path, which it writes to its standard output.
	stdoutReader := bufio.NewReader(w.outPipe)
	stderrReader := bufio.NewReader(w.errPipe)

	var errDone sync.WaitGroup
	errDone.Add(1)
	go func() {
		defer errDone.Done()
		for {
			line, err := stderrReader.ReadString('\n')
			if err != nil {
				log.WithError(err).Info("End of workload stderr")
				return
			}
			_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "%v[stderr] %v", w.Name, line)
		}
	}()

	pid, err := stdoutReader.ReadString('\n')
	if err != nil {
		// (Only) if we fail here, wait for the stderr to be output before returning.
		defer errDone.Wait()
		return fmt.Errorf("reading PID from stdout failed: %w", err)
	}
	w.pid = strings.TrimSpace(pid)

	namespacePath, err := stdoutReader.ReadString('\n')
	if err != nil {
		// (Only) if we fail here, wait for the stderr to be output before returning.
		defer errDone.Wait()
		return fmt.Errorf("reading from stdout failed: %w", err)
	}
	log.WithField("workload", w.Name).Infof("Workload namespace path: %s", namespacePath)

	w.namespacePath = strings.TrimSpace(namespacePath)

	go func() {
		for {
			line, err := stdoutReader.ReadString('\n')
			if err != nil {
				log.WithError(err).Info("End of workload stdout")
				return
			}
			_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "%v[stdout] %v", w.Name, line)
		}
	}()

	w.isRunning = true
	log.WithField("workload", w).Info("Workload now running")

	return nil
}

func (w *Workload) IPNet() string {
	return w.IP + "/32"
}

// AddSpoofInterface adds a second interface to the workload with name Workload.SpoofIfaceName and moves the
// workload's IP to its loopback so that we can maintain a TCP connection while moving routes between the two
// interfaces. From the host's point of view, this looks like one interface is trying to hijack the connection of
// the other.
func (w *Workload) AddSpoofInterface() {
	// If the host container, add a new veth pair.
	w.C.Exec("ip", "link", "add", "name", w.SpoofInterfaceName, "type", "veth", "peer", "name", "spoof0")
	w.C.Exec("ip", "link", "set", w.SpoofInterfaceName, "addr", "ee:ee:ee:ee:ee:ee")
	w.C.Exec("ip", "link", "set", "up", w.SpoofInterfaceName)
	// Move one end of the veth into the workload netns.
	w.C.Exec("ip", "link", "set", "spoof0", "netns", w.netns())
	// In the workload netns, bring up the new interface and then move the IP to the loopback.
	w.Exec("ip", "link", "set", "up", "spoof0")
	w.Exec("ip", "addr", "del", w.IP, "dev", "eth0")
	w.Exec("ip", "addr", "add", w.IP, "dev", "lo")
	// Recreate the routes, which get removed when we remove the address.
	w.Exec("ip", "route", "add", "169.254.169.254/32", "dev", "eth0")
	w.Exec("ip", "route", "add", "default", "via", "169.254.169.254")
	// Add static ARP entry, otherwise connections fail at the ARP stage because the host won't respond.
	w.Exec("arp", "-i", "spoof0", "-s", "169.254.169.254", "ee:ee:ee:ee:ee:ee")

	w.isSpoofing = true
}

func (w *Workload) UseSpoofInterface(spoof bool) {
	var iface string
	if spoof {
		iface = "spoof0"
	} else {
		iface = "eth0"
	}
	w.Exec("ip", "route", "replace", "169.254.169.254/32", "dev", iface)
	w.Exec("ip", "route", "replace", "default", "via", "169.254.169.254", "dev", iface)
}

// Configure creates a workload endpoint in the datastore.
// Deprecated: should use ConfigureInInfra.
func (w *Workload) Configure(client client.Interface) {
	wep := w.WorkloadEndpoint
	wep.Namespace = "fv"
	var err error
	w.WorkloadEndpoint, err = client.WorkloadEndpoints().Create(utils.Ctx, w.WorkloadEndpoint, utils.NoOptions)
	Expect(err).NotTo(HaveOccurred(), "Failed to create workload in the calico datastore.")
}

// RemoveFromDatastore removes the workload endpoint from the datastore.
// Deprecated: should use RemoveFromInfra.
func (w *Workload) RemoveFromDatastore(client client.Interface) {
	_, err := client.WorkloadEndpoints().Delete(utils.Ctx, "fv", w.WorkloadEndpoint.Name, options.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

// ConfigureInInfra creates the workload endpoint for this Workload.
func (w *Workload) ConfigureInInfra(infra infrastructure.DatastoreInfra) {
	wep := w.WorkloadEndpoint
	if wep.Namespace == "" {
		wep.Namespace = "default"
	}
	wep.Spec.Workload = w.Name
	wep.Spec.Endpoint = w.Name
	wep.Spec.InterfaceName = w.InterfaceName
	var err error
	w.WorkloadEndpoint, err = infra.AddWorkload(wep)
	Expect(err).NotTo(HaveOccurred(), "Failed to add workload")
}

// UpdateInInfra updates the workload endpoint for this Workload.
func (w *Workload) UpdateInInfra(infra infrastructure.DatastoreInfra) {
	wep := w.WorkloadEndpoint
	if wep.Namespace == "" {
		wep.Namespace = "default"
	}
	wep.Spec.Workload = w.Name
	wep.Spec.Endpoint = w.Name
	wep.Spec.InterfaceName = w.InterfaceName
	var err error
	w.WorkloadEndpoint, err = infra.UpdateWorkload(wep)
	log.WithFields(log.Fields{"Workload": w, "WorkloadEndpoint": w.WorkloadEndpoint, "QoSControls": w.WorkloadEndpoint.Spec.QoSControls}).Infof("Update WorkloadEndpoint")
	Expect(err).NotTo(HaveOccurred(), "Failed to update workload")
}

// ConfigureInInfraAsSpoofInterface creates a valid workload endpoint for this Workload, using the spoof interface
// added with AddSpoofInterface. After calling AddSpoofInterface(), UseSpoofInterface(true), and, this method,
// connectivity should work because the workload and felix will agree on the interface that should be used.
func (w *Workload) ConfigureInInfraAsSpoofInterface(infra infrastructure.DatastoreInfra) {
	wep := w.WorkloadEndpoint.DeepCopy()
	wep.Namespace = "default"
	wep.Spec.Workload = w.SpoofName
	wep.Spec.Endpoint = w.SpoofName
	wep.Spec.InterfaceName = w.SpoofInterfaceName
	var err error
	w.SpoofWorkloadEndpoint, err = infra.AddWorkload(wep)
	Expect(err).NotTo(HaveOccurred(), "Failed to add workload")
}

// ConfigureOtherWEPInInfraAsSpoofInterface creates a WEP for the spoof interface that does not match this Workload's
// IP address.
func (w *Workload) ConfigureOtherWEPInInfraAsSpoofInterface(infra infrastructure.DatastoreInfra) {
	wep := w.WorkloadEndpoint.DeepCopy()
	wep.Namespace = "default"
	wep.Spec.Workload = w.SpoofName
	wep.Spec.Endpoint = w.SpoofName
	wep.Spec.InterfaceName = w.SpoofInterfaceName
	wep.Spec.IPNetworks[0] = "1.2.3.4"
	var err error
	w.SpoofWorkloadEndpoint, err = infra.AddWorkload(wep)
	Expect(err).NotTo(HaveOccurred(), "Failed to add workload")
}

// RemoveSpoofWEPFromInfra removes the spoof WEP created by ConfigureInInfraAsSpoofInterface or
// ConfigureOtherWEPInInfraAsSpoofInterface.
func (w *Workload) RemoveSpoofWEPFromInfra(infra infrastructure.DatastoreInfra) {
	err := infra.RemoveWorkload(w.SpoofWorkloadEndpoint.Namespace, w.SpoofWorkloadEndpoint.Name)
	Expect(err).NotTo(HaveOccurred(), "Failed to remove workload")
}

// RemoveFromInfra removes the WEP created by ConfigureInInfra.
func (w *Workload) RemoveFromInfra(infra infrastructure.DatastoreInfra) {
	err := infra.RemoveWorkload(w.WorkloadEndpoint.Namespace, w.WorkloadEndpoint.Name)
	Expect(err).NotTo(HaveOccurred(), "Failed to remove workload")
}

func (w *Workload) NameSelector() string {
	return "name=='" + w.Name + "'"
}

func (w *Workload) SourceName() string {
	return w.Name
}

func (w *Workload) SourceIPs() []string {
	ips := []string{}
	if w.IP != "" {
		ips = append(ips, w.IP)
	}
	if w.IP6 != "" {
		ips = append(ips, w.IP6)
	}
	return ips
}

func (w *Workload) PreRetryCleanup(ip, port, protocol string, opts ...connectivity.CheckOption) {
	anyPort := w.conncheckAnyPort()
	anyPort.PreRetryCleanup(ip, port, protocol, opts...)
}

func (w *Workload) CanConnectTo(ip, port, protocol string, opts ...connectivity.CheckOption) *connectivity.Result {
	anyPort := w.conncheckAnyPort()
	return anyPort.CanConnectTo(ip, port, protocol, opts...)
}

func (w *Workload) conncheckAnyPort() Port {
	anyPort := Port{
		Workload: w,
	}
	return anyPort
}

func (w *Workload) Port(port uint16) *Port {
	return &Port{
		Workload: w,
		Port:     port,
	}
}

func (w *Workload) NamespaceID() string {
	splits := strings.Split(w.namespacePath, "/")
	return splits[len(splits)-1]
}

func (w *Workload) NamespacePath() string {
	return w.namespacePath
}

func (w *Workload) Exec(args ...string) {
	out, err := w.ExecCombinedOutput(args...)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Exec of %v failed; output: %s", args, out))
}

func (w *Workload) ExecOutput(args ...string) (string, error) {
	args = append([]string{"ip", "netns", "exec", w.NamespaceID()}, args...)
	return w.C.ExecOutput(args...)
}

func (w *Workload) ExecCombinedOutput(args ...string) (string, error) {
	args = append([]string{"ip", "netns", "exec", w.NamespaceID()}, args...)
	return w.C.ExecCombinedOutput(args...)
}

var rttRegexp = regexp.MustCompile(`rtt=(.*) ms`)

func (w *Workload) LatencyTo(ip, port string) (time.Duration, string) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}
	out, err := w.ExecOutput("hping3", "-p", port, "-c", "20", "--fast", "-S", "-n", ip)
	stderr := ""
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr = string(exitErr.Stderr)
	}
	Expect(err).NotTo(HaveOccurred(), stderr)

	lines := strings.Split(out, "\n")[1:] // Skip header line
	var rttSum time.Duration
	var numBuggyRTTs int
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		matches := rttRegexp.FindStringSubmatch(line)
		Expect(matches).To(HaveLen(2), "Failed to extract RTT from line: "+line)
		rttMsecStr := matches[1]
		rttMsec, err := strconv.ParseFloat(rttMsecStr, 64)
		Expect(err).ToNot(HaveOccurred())
		if rttMsec > 1000 {
			// There's a bug in hping where it occasionally reports RTT+1s instead of RTT.  Work around that
			// but keep track of the number of workarounds and bail out if we see too many.
			rttMsec -= 1000
			numBuggyRTTs++
		}
		rttSum += time.Duration(rttMsec * float64(time.Millisecond))
	}
	Expect(numBuggyRTTs).To(BeNumerically("<", len(lines)/2),
		"hping reported a large number of >1s RTTs; full output:\n"+out)
	meanRtt := rttSum / time.Duration(len(lines))
	return meanRtt, out
}

func (w *Workload) SendPacketsTo(ip string, count int, size int) (error, string) {
	c := fmt.Sprintf("%d", count)
	s := fmt.Sprintf("%d", size)
	_, err := w.ExecOutput("ping", "-c", c, "-W", "1", "-s", s, ip)
	stderr := ""
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr = string(exitErr.Stderr)
	}
	return err, stderr
}

type SideService struct {
	W       *Workload
	Name    string
	RunCmd  *exec.Cmd
	PidFile string
}

func (s *SideService) Stop() {
	Expect(s.stop()).NotTo(HaveOccurred())
}

func (s *SideService) stop() error {
	log.WithField("SideService", s).Info("Stop")
	output, err := s.W.C.ExecOutput("cat", s.PidFile)
	if err != nil {
		log.WithField("pidfile", s.PidFile).WithError(err).Warn("Failed to get contents of a side service's pidfile")
		return err
	}
	pid := strings.TrimSpace(output)
	err = s.W.C.ExecMayFail("kill", pid)
	if err != nil {
		log.WithField("pid", pid).WithError(err).Warn("Failed to kill a side service")
		return err
	}
	_, err = s.RunCmd.Process.Wait()
	if err != nil {
		log.WithField("side service", s).Error("failed to wait for process")
	}

	log.WithField("SideService", s).Info("Side service now stopped")
	return nil
}

func (w *Workload) StartSideService() *SideService {
	s, err := startSideService(w)
	Expect(err).NotTo(HaveOccurred())
	return s
}

func startSideService(w *Workload) (*SideService, error) {
	sideServIdx++
	n := fmt.Sprintf("%s-ss%d", w.Name, sideServIdx)
	pidFile := fmt.Sprintf("/tmp/%s-pid", n)

	testWorkloadShArgs := []string{
		"test-workload",
	}
	if w.Protocol == "udp" {
		testWorkloadShArgs = append(testWorkloadShArgs, "--udp")
	}
	testWorkloadShArgs = append(testWorkloadShArgs,
		"--sidecar-iptables",
		fmt.Sprintf("'--namespace-path=%s'", w.namespacePath),
		"''", // interface name, not important
		"127.0.0.1",
		"15001",
	)
	pidCmd := fmt.Sprintf("echo $$ >'%s'", pidFile)
	testWorkloadCmd := strings.Join(testWorkloadShArgs, " ")
	dockerWorkloadArgs := []string{
		"docker",
		"exec",
		w.C.Name,
		"sh", "-c",
		fmt.Sprintf("%s; exec %s", pidCmd, testWorkloadCmd),
	}
	runCmd := utils.Command(dockerWorkloadArgs[0], dockerWorkloadArgs[1:]...)
	logName := fmt.Sprintf("side service %s", n)
	if err := utils.LogOutput(runCmd, logName); err != nil {
		return nil, fmt.Errorf("failed to start output logging for %s", logName)
	}
	if err := runCmd.Start(); err != nil {
		return nil, fmt.Errorf("starting /test-workload as a side service failed: %v", err)
	}
	return &SideService{
		W:       w,
		Name:    n,
		RunCmd:  runCmd,
		PidFile: pidFile,
	}, nil
}

type PersistentConnectionOpts struct {
	SourcePort          int
	MonitorConnectivity bool
	Timeout             time.Duration
}

func (w *Workload) StartPersistentConnectionMayFail(
	ip string, port int,
	opts PersistentConnectionOpts,
) (*connectivity.PersistentConnection, error) {
	pc := &connectivity.PersistentConnection{
		RuntimeName:         w.C.Name,
		Runtime:             w.C,
		IP:                  ip,
		Port:                port,
		Protocol:            w.Protocol,
		NamespacePath:       w.namespacePath,
		SourcePort:          opts.SourcePort,
		MonitorConnectivity: opts.MonitorConnectivity,
		Timeout:             opts.Timeout,
	}

	err := pc.Start()

	return pc, err
}

func (w *Workload) StartPersistentConnection(
	ip string, port int,
	opts PersistentConnectionOpts,
) *connectivity.PersistentConnection {
	pc, err := w.StartPersistentConnectionMayFail(ip, port, opts)
	Expect(err).NotTo(HaveOccurred())

	return pc
}

func (w *Workload) ToMatcher(explicitPort ...uint16) *connectivity.Matcher {
	var port string
	if len(explicitPort) == 1 {
		port = fmt.Sprintf("%d", explicitPort[0])
	} else if w.DefaultPort != "" {
		port = w.DefaultPort
	} else if !strings.Contains(w.Ports, ",") {
		port = w.Ports
	} else {
		panic("Explicit port needed for workload with multiple ports")
	}
	return &connectivity.Matcher{
		IP:         w.IP,
		IP6:        w.IP6,
		Port:       port,
		TargetName: fmt.Sprintf("%s on port %s", w.Name, port),
		Protocol:   "tcp",
	}
}

const nsprefix = "/var/run/netns/"

func (w *Workload) netns() string {
	if strings.HasPrefix(w.namespacePath, nsprefix) {
		return strings.TrimPrefix(w.namespacePath, nsprefix)
	}

	return ""
}

func (w *Workload) RunCmd(cmd string, args ...string) (string, error) {
	netns := w.netns()
	dockerArgs := []string{"exec", w.C.Name}
	if netns != "" {
		dockerArgs = append(dockerArgs, "ip", "netns", "exec", netns)
	}
	dockerArgs = append(dockerArgs, cmd)
	dockerArgs = append(dockerArgs, args...)
	dockerCmd := utils.Command("docker", dockerArgs...)
	out, err := dockerCmd.CombinedOutput()

	log.WithField("output", string(out)).Debug("Workload.RunCmd")
	return string(out), err
}

func (w *Workload) PathMTU(ip string) (int, error) {
	out, err := w.RunCmd("ip", "route", "show", "cached")
	if err != nil {
		return 0, err
	}

	outRd := bufio.NewReader(strings.NewReader(out))
	ipRegex := regexp.MustCompile("^" + ip + ".*")
	mtuRegex := regexp.MustCompile(".*mtu ([0-9]+)")
	for {
		line, err := outRd.ReadString('\n')
		if err != nil {
			return 0, nil
		}
		if ipRegex.MatchString(line) {
			line, err := outRd.ReadString('\n')
			if err != nil {
				return 0, nil
			}
			m := mtuRegex.FindStringSubmatch(line)
			if len(m) == 0 {
				return 0, nil
			}
			return strconv.Atoi(m[1])
		}
	}
}

// AttachTCPDump returns tcpdump attached to the workload
func (w *Workload) AttachTCPDump() *tcpdump.TCPDump {
	netns := w.netns()
	tcpd := tcpdump.Attach(w.C.Name, netns, "eth0")
	tcpd.SetLogString(w.Name)
	return tcpd
}

type SpoofedWorkload struct {
	*Workload
	SpoofedSourceIP string
}

func (s *SpoofedWorkload) PreRetryCleanup(ip, port, protocol string, opts ...connectivity.CheckOption) {
	opts = s.appendSourceIPOpt(opts)
	s.Workload.preRetryCleanupInner(ip, port, protocol, "(spoofed)", opts...)
}

func (s *SpoofedWorkload) CanConnectTo(ip, port, protocol string, opts ...connectivity.CheckOption) *connectivity.Result {
	opts = s.appendSourceIPOpt(opts)
	return s.Workload.canConnectToInner(ip, port, protocol, "(spoofed)", opts...)
}

func (s *SpoofedWorkload) appendSourceIPOpt(opts []connectivity.CheckOption) []connectivity.CheckOption {
	opts = append(opts, connectivity.WithSourceIP(s.SpoofedSourceIP))
	return opts
}

type Port struct {
	*Workload
	Port uint16
}

func (p *Port) SourceName() string {
	if p.Port == 0 {
		return p.Name
	}
	return fmt.Sprintf("%s:%d", p.Name, p.Port)
}

func (p *Port) SourceIPs() []string {
	return []string{p.IP}
}

func (p *Port) PreRetryCleanup(ip, port, protocol string, opts ...connectivity.CheckOption) {
	opts = p.maybeAppendPortOpt(opts)
	p.Workload.preRetryCleanupInner(ip, port, protocol, "(with source port)", opts...)
}

// Return if a connection is good and packet loss string "PacketLoss[xx]".
// If it is not a packet loss test, packet loss string is "".
func (p *Port) CanConnectTo(ip, port, protocol string, opts ...connectivity.CheckOption) *connectivity.Result {
	opts = p.maybeAppendPortOpt(opts)
	return p.Workload.canConnectToInner(ip, port, protocol, "(with source port)", opts...)
}

func (p *Port) maybeAppendPortOpt(opts []connectivity.CheckOption) []connectivity.CheckOption {
	if p.Port != 0 {
		opts = append(opts, connectivity.WithSourcePort(strconv.Itoa(int(p.Port))))
	}
	return opts
}

func (w *Workload) preRetryCleanupInner(ip, port, protocol, logSuffix string, opts ...connectivity.CheckOption) {
	if protocol == "udp" || protocol == "sctp" {
		// Defensive, we might get called in parallel for different ports, avoid trying to run
		// clashing cleanup commands at the same time.
		w.cleanupLock.Lock()
		defer w.cleanupLock.Unlock()

		// If this is a retry then we may have stale conntrack entries and we don't want those
		// to influence the connectivity check.  UDP lacks a sequence number, so conntrack operates
		// on a simple timer. In the case of SCTP, conntrack appears to match packets even when
		// the conntrack entry is in the CLOSED state.
		if os.Getenv("FELIX_FV_ENABLE_BPF") == "true" {
			w.C.Exec("calico-bpf", "conntrack", "remove", "udp", w.IP, ip)
		} else {
			_ = w.C.ExecMayFail("conntrack", "-D", "-p", protocol, "-s", w.IP, "-d", ip)
		}
	}
}

func (w *Workload) canConnectToInner(ip, port, protocol, logSuffix string, opts ...connectivity.CheckOption) *connectivity.Result {
	logMsg := "Connection test"

	// enforce the name space as we want to execute it in the workload
	opts = append(opts, connectivity.WithNamespacePath(w.namespacePath))
	logMsg += " " + logSuffix

	return connectivity.Check(w.C.Name, logMsg, ip, port, protocol, opts...)
}

// ToMatcher implements the connectionTarget interface, allowing this port to be used as
// target.
func (p *Port) ToMatcher(explicitPort ...uint16) *connectivity.Matcher {
	if p.Port == 0 {
		return p.Workload.ToMatcher(explicitPort...)
	}
	return &connectivity.Matcher{
		IP:         p.Workload.IP,
		Port:       fmt.Sprint(p.Port),
		TargetName: fmt.Sprintf("%s on port %d", p.Workload.Name, p.Port),
		IP6:        p.Workload.IP6,
	}
}

func (w *Workload) InterfaceIndex() int {
	out, err := w.C.ExecOutput("ip", "link", "show", "dev", w.InterfaceName)
	Expect(err).NotTo(HaveOccurred())
	ifIndex, err := strconv.Atoi(strings.SplitN(out, ":", 2)[0])
	Expect(err).NotTo(HaveOccurred())
	log.Infof("%v is ifindex %v", w.InterfaceName, ifIndex)
	return ifIndex
}

func (w *Workload) RenameInterface(from, to string) {
	var err error
	sleep := 100 * time.Millisecond
	for try := 0; try < 40; try++ {
		// Can fail with EBUSY.
		err = w.C.ExecMayFail("ip", "link", "set", from, "name", to)
		if err == nil {
			return
		}
		time.Sleep(sleep)
		sleep = time.Duration(float64(sleep) * (1.5 + rand.Float64()))
		const maxSleep = 2 * time.Second
		if sleep > maxSleep {
			sleep = maxSleep
		}
	}
	ginkgo.Fail(fmt.Sprintf("Failed to rename interface %s to %s after several retries: %s", from, to, err))
}

func (w *Workload) SetInterfaceUp(b bool) {
	if b {
		w.C.Exec("ip", "link", "set", "up", w.InterfaceName)
	} else {
		w.C.Exec("ip", "link", "set", "down", w.InterfaceName)
	}
}

func (w *Workload) ExecCommand(name string, args ...string) *exec.Cmd {
	args = append([]string{"exec", w.C.Name, "ip", "netns", "exec", w.NamespaceID(), name}, args...)
	cmd := utils.Command("docker", args...)
	return cmd
}
