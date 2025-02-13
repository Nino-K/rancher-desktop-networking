package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/linuxkit/virtsock/pkg/vsock"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
	"golang.org/x/sync/errgroup"
)

var (
	debug      bool
	childPath  string
	unshareArg string
)

const (
	vsockHandshakePort = 6669
	vsockDialPort      = 6655
	SeedPhrase         = "github.com/rancher-sandbox/rancher-desktop-networking"
)


func main() {
	flag.BoolVar(&debug, "debug", false, "enable debug flag")
	flag.StringVar(&childPath, "child-path", "", "the path to the child sub process that will run in a new namespace")
	flag.StringVar(&unshareArg, "unshare-arg", "", "the arg for unshare program")
	flag.Parse()

	if childPath == "" {
		logrus.Fatal("path to the child sub process must be provided")
	}

	if unshareArg == "" {
		logrus.Fatal("unshare program arg must be provided")
	}

	logrus.Info("Starting handhsake...")

	done := make(chan struct{})
	go listenForHandshake(done)
	<-done

	// figure out a better way to do this
	time.Sleep(time.Second * 5)

	conn, err := vsock.Dial(vsock.CIDHost, vsockDialPort)
	if err != nil {
		logrus.Fatalf("cannot connect to host: %v", err)
	}
	logrus.Debugf("dialed host %v:%d: %+v", vsock.CIDHost, vsockDialPort, conn)

	errGroup := errgroup.Group{}

	// setup network namespace
	_, err = configureNamespace("rd1")
	if err != nil {
		logrus.Fatal(err)
	}

	unshareCmd := exec.Command("/usr/bin/nsenter", "-n/run/netns/rd1", "-F",  "/usr/bin/unshare", "--pid", "--mount-proc", "--fork", "--propagation", "slave", unshareArg)
	unshareCmd.Stdin = os.Stdin
	unshareCmd.Stdout = os.Stdout
	unshareCmd.Stderr = os.Stderr
	if err := unshareCmd.Start(); err != nil {
		logrus.Fatalf("could not start the unshare process: %v", err)
	}

	// /run/wsl-init.pid
	unsharePID := strconv.Itoa(unshareCmd.Process.Pid)

	err = os.WriteFile("/run/wsl-init.pid", []byte(unsharePID), 0644)
	if err != nil{
		logrus.Fatal(err)
	}
	logrus.Infof("unshare pid is: %v", unsharePID)
	logrus.Infof("unshare arg is: %s", unshareArg)

	// start the child process
	// the child process will run in the new namespace the reason behind this subprocess
	// is to avoid swithcing back and forth between namespaces,
	// this is due to the limitaion in the golang's runtime
	// all the sub process could potentiall start in the original
	// namespace
	subProcess := exec.Command(childPath, "-debug")
	logFile, err := os.Create("/var/logs/vm-switch.log")
	if err != nil {
		logrus.Errorf("failed to create log file for vm-switch: %v", err)
	} else {
		subProcess.Stdout = logFile
		subProcess.Stderr = logFile
	}
	connFile, err := conn.File()
	if err != nil {
		logrus.Fatalf("failed to get vsock connection fd: %v", err)
	}
	subProcess.ExtraFiles = []*os.File{ connFile}
	if err := subProcess.Start(); err != nil {
		logrus.Fatalf("could not start the child process: %v", err)
	}
	logFile.Close()

	logrus.Infof("successfully started the child process vm-switch running with a PID: %v", subProcess.Process.Pid)

	errGroup.Go(func() error {
		if err := subProcess.Wait(); err != nil {
			return fmt.Errorf("vm-switch exited with error: %w", err)
		}
		return nil
	})

	if err = errGroup.Wait(); err != nil {
		logrus.Fatal(err)
	}
	logrus.Infof("handshake process done")
}

func listenForHandshake(done chan<- struct{}) {
	l, err := vsock.Listen(vsock.CIDAny, vsockHandshakePort)
	if err != nil {
		logrus.Fatalf("listenForHandshake listen failed: %v", err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
			defer conn.Close()
			if err != nil {
				logrus.Errorf("listenForHandshake connection accept: %v", err)
				return
			}
			_, err = conn.Write([]byte(SeedPhrase))
			if err != nil {
				logrus.Errorf("listenForHandshake writing CIDHost: %v\n", err)
				return
			}
			logrus.Info("successful handshake with host switch")
			done <- struct{}{}
	}

}

func configureNamespace(ns string) (netns.NsHandle, error) {
	// intentionally ignoring this error
	_ = netns.DeleteNamed(ns)

	rdNs, err := netns.NewNamed(ns)
	if err != nil {
		return netns.None(), errors.Wrap(err, "creating new namespace failed")
	}

	logrus.Infof("created a new namespace %v %v", ns, rdNs.String())
	return rdNs, nil
}
