package privileged_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"

	"time"

	"io"
	"io/ioutil"
	"net/http"
	"syscall"

	. "code.cloudfoundry.org/cfdev/acceptance"
	"code.cloudfoundry.org/garden/client"
	"code.cloudfoundry.org/garden/client/connection"
	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/onsi/gomega/gbytes"
)

var _ = Describe("hyperkit lifecycle", func() {
	var (
		cfdevHome       string
		linuxkitPidPath string
		vpnkitPidPath   string
		stateDir        string
		cacheDir        string
	)

	BeforeEach(func() {
		Expect(HasSudoPrivilege()).To(BeTrue(), "Please run 'sudo echo hi' first")
		RemoveIPAliases(BoshDirectorIP, CFRouterIP)
		FullCleanup()

		cfdevHome = os.Getenv("CFDEV_HOME")
		if cfdevHome == "" {
			cfdevHome = filepath.Join(os.Getenv("HOME"), ".cfdev")
			os.Remove(filepath.Join(cfdevHome, "http_proxy.json"))
		}
		cacheDir = filepath.Join(cfdevHome, "cache")
		stateDir = filepath.Join(cfdevHome, "state")
		linuxkitPidPath = filepath.Join(stateDir, "linuxkit.pid")
		vpnkitPidPath = filepath.Join(stateDir, "vpnkit.pid")

		session := cf.Cf("install-plugin", pluginPath, "-f")
		Eventually(session).Should(gexec.Exit(0))
		session = cf.Cf("plugins")
		Eventually(session).Should(gexec.Exit(0))
	})

	AfterEach(func() {
		gexec.KillAndWait()
		FullCleanup()
		RemoveIPAliases(BoshDirectorIP, CFRouterIP)

		session := cf.Cf("dev", "stop")
		Eventually(session).Should(gexec.Exit(0))
	})

	It("runs the entire vm lifecycle", func() {
		session := cf.Cf("dev", "start")
		Eventually(session, 20*time.Minute).Should(gbytes.Say("Starting VPNKit"))

		By("settingup VPNKit dependencies")
		Eventually(filepath.Join(cfdevHome, "http_proxy.json"), 10, 1).Should(BeAnExistingFile())

		Eventually(vpnkitPidPath, 10, 1).Should(BeAnExistingFile())
		Eventually(linuxkitPidPath, 10, 1).Should(BeAnExistingFile())

		// FYI - this will take time until we use thin provisioned disks
		hyperkitPidPath := filepath.Join(stateDir, "hyperkit.pid")
		Eventually(hyperkitPidPath, 120, 1).Should(BeAnExistingFile())

		By("waiting for garden to listen")
		EventuallyShouldListenAt("http://"+GardenIP+":8888", 360)

		EventuallyWeCanTargetTheBOSHDirector()

		By("waiting for cf router to listen")
		EventuallyShouldListenAt("http://"+CFRouterIP+":80", 1200)

		By("waiting for cfdev cli to exit when the deploy finished")
		Eventually(session, 300).Should(gexec.Exit(0))

		linuxkitPid := PidFromFile(linuxkitPidPath)
		hyperkitPid := PidFromFile(hyperkitPidPath)
		vpnkitPid := PidFromFile(vpnkitPidPath)

		By("deploy finished - stopping...")
		session = cf.Cf("dev", "stop")
		Eventually(session).Should(gexec.Exit(0))

		//ensure pid is not running
		EventuallyProcessStops(linuxkitPid, 5)
		EventuallyProcessStops(hyperkitPid, 5)
		EventuallyProcessStops(vpnkitPid, 5)
	})

	Context("Run with", func() {
		var assetUrl = "https://s3.amazonaws.com/cfdev-test-assets/test-deps.dev"
		var assetDir string

		BeforeEach(func() {
			var err error
			assetDir, err = ioutil.TempDir(os.TempDir(), "asset")
			Expect(err).ToNot(HaveOccurred())
			downloadTestAsset(assetDir, assetUrl)
		})

		AfterEach(func() {
			err := os.RemoveAll(assetDir)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Custom ISO", func() {
			session := cf.Cf("dev", "start", "-f", filepath.Join(assetDir, "test-deps.dev"))
			Eventually(session, 20*time.Minute).Should(gbytes.Say("Starting VPNKit"))

			By("settingup VPNKit dependencies")
			Eventually(filepath.Join(cfdevHome, "http_proxy.json"), 10, 1).Should(BeAnExistingFile())
			Eventually(vpnkitPidPath, 10, 1).Should(BeAnExistingFile())
			Eventually(linuxkitPidPath, 10, 1).Should(BeAnExistingFile())

			hyperkitPidPath := filepath.Join(stateDir, "hyperkit.pid")
			Eventually(hyperkitPidPath, 120, 1).Should(BeAnExistingFile())

			By("waiting for garden to listen")
			EventuallyShouldListenAt("http://"+GardenIP+":8888", 360)

			client := client.New(connection.New("tcp", "localhost:8888"))
			Eventually(func() (string, error) {
				return GetFile(client, "deploy-bosh", "/var/vcap/cache/test-file-one.txt")
			}).Should(Equal("testfileone\n"))

			session.Terminate()
			Eventually(session).Should(gexec.Exit())

			By("deploy finished - stopping...")
			session = cf.Cf("dev", "stop")
			Eventually(session).Should(gexec.Exit(0))
		})
	})
})

func EventuallyWeCanTargetTheBOSHDirector() {
	By("waiting for bosh to listen")
	EventuallyShouldListenAt("https://"+BoshDirectorIP+":25555", 480)

	// Even though the test below is very similar this fails fast when `bosh env`
	// command is broken

	session := cf.Cf("dev", "bosh", "env")
	Eventually(session).Should(gexec.Exit(0))

	// This test is more representative of how `bosh env` should be invoked
	w := gexec.NewPrefixedWriter("[bosh env] ", GinkgoWriter)
	boshCmd := exec.Command("/bin/sh",
		"-e",
		"-c", fmt.Sprintf(`eval "$(cf dev bosh env)" && bosh env`))

	session, err := gexec.Start(boshCmd, w, w)
	Expect(err).ToNot(HaveOccurred())
	Eventually(session, 30, 1).Should(gexec.Exit(0))
}

func RemoveIPAliases(aliases ...string) {
	for _, alias := range aliases {
		cmd := exec.Command("sudo", "-n", "ifconfig", "lo0", "inet", alias+"/32", "remove")
		writer := gexec.NewPrefixedWriter("[ifconfig] ", GinkgoWriter)
		session, err := gexec.Start(cmd, writer, writer)
		Expect(err).ToNot(HaveOccurred())
		Eventually(session).Should(gexec.Exit())
	}
}

func downloadTestAsset(targetDir string, resourceUrl string) error {
	out, err := os.Create(filepath.Join(targetDir, "test-deps.dev"))
	if err != nil {
		return err
	}
	defer out.Close()

	resp, err := http.Get(resourceUrl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

func FullCleanup() {
	out, err := exec.Command("ps", "aux").Output()
	Expect(err).NotTo(HaveOccurred())
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "linuxkit") || strings.Contains(line, "hyperkit") || strings.Contains(line, "vpnkit") {
			cols := strings.Fields(line)
			pid, err := strconv.Atoi(cols[1])
			if err == nil && pid > 0 {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	out, err = exec.Command("ps", "aux").Output()
	Expect(err).NotTo(HaveOccurred())
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "linuxkit") || strings.Contains(line, "hyperkit") || strings.Contains(line, "vpnkit") {
			Fail(fmt.Sprintf("one of the 'kits' was still running: %s", line))
		}
	}
}
