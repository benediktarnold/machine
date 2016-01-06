package provision

import (
	"bytes"
	"fmt"
	"text/template"
	"time"

	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/provision/pkgaction"
	"github.com/docker/machine/libmachine/provision/serviceaction"
	"github.com/docker/machine/libmachine/swarm"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnutils"
)

func init() {
	Register("Hypriot", &RegisteredProvisioner{
		New: NewHypriotProvisioner,
	})
}

func NewHypriotProvisioner(d drivers.Driver) Provisioner {
	return &HypriotProvisioner{
		GenericProvisioner{
			SSHCommander:      GenericSSHCommander{Driver: d},
			DockerOptionsDir:  "/etc/docker",
			DaemonOptionsFile: "/etc/default/docker",
			OsReleaseID:       "raspbian",
			Packages:          []string{},
			Driver:            d,
		},
	}
}

type HypriotProvisioner struct {
	GenericProvisioner
}

func (provisioner *HypriotProvisioner) String() string {
	return "hypriot"
}

func (provisioner *HypriotProvisioner) CompatibleWithHost() bool {
	if _, err := provisioner.SSHCommand("cat /etc/hypriot_release"); err != nil {
		return false
	}

	return provisioner.OsReleaseInfo.ID == provisioner.OsReleaseID
}

func (provisioner *HypriotProvisioner) Service(name string, action serviceaction.ServiceAction) error {
	command := fmt.Sprintf("sudo service %s %s", name, action.String())

	if _, err := provisioner.SSHCommand(command); err != nil {
		return err
	}

	return nil
}

func (provisioner *HypriotProvisioner) Package(name string, action pkgaction.PackageAction) error {
	var (
		packageAction  string
		updateMetadata = true
	)

	switch action {
	case pkgaction.Install:
		packageAction = "install"
	case pkgaction.Remove:
		packageAction = "remove"
		updateMetadata = false
	case pkgaction.Upgrade:
		packageAction = "install"
	}

	switch name {
	case "docker":
		name = "docker-hypriot"
	}

	if updateMetadata {
		// invoke apt-get update for metadata
		if _, err := provisioner.SSHCommand("sudo -E apt-get update"); err != nil {
			return err
		}
	}

	command := fmt.Sprintf("DEBIAN_FRONTEND=noninteractive sudo -E apt-get %s -y %s", packageAction, name)

	if _, err := provisioner.SSHCommand(command); err != nil {
		return err
	}

	return nil
}

func (provisioner *HypriotProvisioner) dockerDaemonResponding() bool {
	if _, err := provisioner.SSHCommand("sudo docker version"); err != nil {
		log.Warnf("Error getting SSH command to check if the daemon is up: %s", err)
		return false
	}

	// The daemon is up if the command worked.  Carry on.
	return true
}

func (provisioner *HypriotProvisioner) dockerDaemonInstalled() bool {
	if _, err := provisioner.SSHCommand("type docker"); err != nil {
		log.Warnf("Docker not installed, let's install it")
		return false
	}

	return true
}

func (provisioner *HypriotProvisioner) dockerDaemonRunning() bool {
	if _, err := provisioner.SSHCommand("sudo service docker status"); err != nil {
		log.Warnf("Docker not running")
		return false
	}

	return true
}

func (provisioner *HypriotProvisioner) setHostnameHypriot(hostname string) error {
	if _, err := provisioner.SSHCommand(fmt.Sprintf(
		"if [ -f /boot/occidentalis.txt ]; then sudo sed -i 's/^hostname.*=.*/hostname=%s/g' /boot/occidentalis.txt; fi",
		hostname,
	)); err != nil {
		return err
	}

	return nil
}

func (provisioner *HypriotProvisioner) setHypriotAptRepo() error {
	if _, err := provisioner.SSHCommand("if [ ! -f /etc/apt/sources.list.d/hypriot.list ] || grep -q repository.hypriot.com /etc/apt/sources.list.d/hypriot.list; then (curl https://packagecloud.io/gpg.key | sudo apt-key add -); echo 'deb https://packagecloud.io/Hypriot/Schatzkiste/debian/ wheezy main' | sudo tee /etc/apt/sources.list.d/hypriot.list; fi"); err != nil {
		return err
	}

	return nil
}

func (provisioner *HypriotProvisioner) Provision(swarmOptions swarm.Options, authOptions auth.Options, engineOptions engine.Options) error {
	provisioner.SwarmOptions = swarmOptions
	provisioner.AuthOptions = authOptions
	provisioner.EngineOptions = engineOptions

	if provisioner.EngineOptions.StorageDriver == "" {
		provisioner.EngineOptions.StorageDriver = "overlay"
	}

	log.Debug("setting hostname")
	if err := provisioner.SetHostname(provisioner.Driver.GetMachineName()); err != nil {
		return err
	}

	if err := provisioner.setHostnameHypriot(provisioner.Driver.GetMachineName()); err != nil {
		return err
	}

	log.Debug("installing apt-transport-https")
	if err := provisioner.Package("apt-transport-https", pkgaction.Install); err != nil {
		return err
	}

	log.Debug("setting Hypriot APT repo")
	if err := provisioner.setHypriotAptRepo(); err != nil {
		return err
	}

	if !provisioner.dockerDaemonInstalled() {
		provisioner.Packages = append(provisioner.Packages, "docker")
	}

	for _, pkg := range provisioner.Packages {
		if err := provisioner.Package(pkg, pkgaction.Install); err != nil {
			return err
		}
	}

	if !provisioner.dockerDaemonRunning() {
		if err := provisioner.Service("docker", serviceaction.Start); err != nil {
			return err
		}
	}

	log.Debug("waiting for docker daemon")
	if err := mcnutils.WaitFor(provisioner.dockerDaemonResponding); err != nil {
		return err
	}

	if err := makeDockerOptionsDir(provisioner); err != nil {
		return err
	}

	provisioner.AuthOptions = setRemoteAuthOptions(provisioner)

	log.Debug("configuring auth")
	if err := ConfigureAuth(provisioner); err != nil {
		return err
	}

	time.Sleep(2 * time.Second)

	log.Debug("configuring swarm")
	if swarmOptions.Image == "swarm:latest" {
		swarmOptions.Image = "hypriot/rpi-swarm:latest"
	}
	log.Debug("swarmOptions.Image = %s", swarmOptions.Image)
	if err := configureSwarm(provisioner, swarmOptions, provisioner.AuthOptions); err != nil {
		return err
	}

	return nil
}

func (provisioner *HypriotProvisioner) GenerateDockerOptions(dockerPort int) (*DockerOptions, error) {
	var (
		engineCfg bytes.Buffer
	)

	driverNameLabel := fmt.Sprintf("provider=%s", provisioner.Driver.DriverName())
	provisioner.EngineOptions.Labels = append(provisioner.EngineOptions.Labels, driverNameLabel)

	engineConfigTmpl := `
DOCKER_OPTS='-H tcp://0.0.0.0:{{.DockerPort}} -H unix:///var/run/docker.sock --storage-driver {{.EngineOptions.StorageDriver}} --tlsverify --tlscacert {{.AuthOptions.CaCertRemotePath}} --tlscert {{.AuthOptions.ServerCertRemotePath}} --tlskey {{.AuthOptions.ServerKeyRemotePath}} {{ range .EngineOptions.Labels }}--label {{.}} {{ end }}{{ range .EngineOptions.InsecureRegistry }}--insecure-registry {{.}} {{ end }}{{ range .EngineOptions.RegistryMirror }}--registry-mirror {{.}} {{ end }}{{ range .EngineOptions.ArbitraryFlags }}--{{.}} {{ end }}'
`
	t, err := template.New("engineConfig").Parse(engineConfigTmpl)
	if err != nil {
		return nil, err
	}

	engineConfigContext := EngineConfigContext{
		DockerPort:    dockerPort,
		AuthOptions:   provisioner.AuthOptions,
		EngineOptions: provisioner.EngineOptions,
	}

	t.Execute(&engineCfg, engineConfigContext)

	return &DockerOptions{
		EngineOptions:     engineCfg.String(),
		EngineOptionsPath: provisioner.DaemonOptionsFile,
	}, nil
}