package libmachine

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/machine/drivers"
	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/provision"
	"github.com/docker/machine/libmachine/swarm"
	"github.com/docker/machine/ssh"
	"github.com/docker/machine/state"
	"github.com/docker/machine/utils"
)

var (
	validHostNameChars   = `[a-zA-Z0-9\-\.]`
	validHostNamePattern = regexp.MustCompile(`^` + validHostNameChars + `+$`)
)

type Host struct {
	Name        string `json:"-"`
	DriverName  string
	Driver      drivers.Driver
	StorePath   string
	HostOptions *HostOptions

	// deprecated options; these are left to assist in config migrations
	SwarmHost      string
	SwarmMaster    bool
	SwarmDiscovery string
	CaCertPath     string
	PrivateKeyPath string
	ServerCertPath string
	ServerKeyPath  string
	ClientCertPath string
	ClientKeyPath  string
}

type HostOptions struct {
	Driver        string
	Memory        int
	Disk          int
	EngineOptions *engine.EngineOptions
	SwarmOptions  *swarm.SwarmOptions
	AuthOptions   *auth.AuthOptions
}

type HostMetadata struct {
	DriverName     string
	HostOptions    HostOptions
	StorePath      string
	CaCertPath     string
	PrivateKeyPath string
	ServerCertPath string
	ServerKeyPath  string
	ClientCertPath string
}

func NewHost(name, driverName string, hostOptions *HostOptions) (*Host, error) {
	authOptions := hostOptions.AuthOptions
	storePath := filepath.Join(utils.GetMachineDir(), name)
	driver, err := drivers.NewDriver(driverName, name, storePath, authOptions.CaCertPath, authOptions.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	return &Host{
		Name:        name,
		DriverName:  driverName,
		Driver:      driver,
		StorePath:   storePath,
		HostOptions: hostOptions,
	}, nil
}

func LoadHost(name string, StorePath string) (*Host, error) {
	if _, err := os.Stat(StorePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("Host %q does not exist", name)
	}

	host := &Host{Name: name, StorePath: StorePath}
	if err := host.LoadConfig(); err != nil {
		return nil, err
	}
	return host, nil
}

func ValidateHostName(name string) bool {
	return validHostNamePattern.MatchString(name)
}

func (h *Host) Create(name string) error {
	// create the instance
	if err := h.Driver.Create(); err != nil {
		return err
	}

	// save to store
	if err := h.SaveConfig(); err != nil {
		return err
	}

	// TODO: Not really a fan of just checking "none" here.
	if h.Driver.DriverName() != "none" {
		if err := WaitForSSH(h); err != nil {
			return err
		}

		provisioner, err := provision.DetectProvisioner(h.Driver)
		if err != nil {
			return err
		}

		if err := provisioner.Provision(*h.HostOptions.SwarmOptions, *h.HostOptions.AuthOptions); err != nil {
			return err
		}
	}

	return nil
}

func (h *Host) GetSSHCommand(args ...string) (*exec.Cmd, error) {
	addr, err := h.Driver.GetSSHHostname()
	if err != nil {
		return nil, err
	}

	user := h.Driver.GetSSHUsername()

	port, err := h.Driver.GetSSHPort()
	if err != nil {
		return nil, err
	}

	keyPath := h.Driver.GetSSHKeyPath()

	cmd := ssh.GetSSHCommand(addr, port, user, keyPath, args...)
	return cmd, nil
}

func (h *Host) MachineInState(desiredState state.State) func() bool {
	return func() bool {
		currentState, err := h.Driver.GetState()
		if err != nil {
			log.Debugf("Error getting machine state: %s", err)
		}
		if currentState == desiredState {
			return true
		}
		return false
	}
}

func (h *Host) Start() error {
	if err := h.Driver.Start(); err != nil {
		return err
	}

	if err := h.SaveConfig(); err != nil {
		return err
	}

	return utils.WaitFor(h.MachineInState(state.Running))
}

func (h *Host) Stop() error {
	if err := h.Driver.Stop(); err != nil {
		return err
	}

	if err := h.SaveConfig(); err != nil {
		return err
	}

	return utils.WaitFor(h.MachineInState(state.Stopped))
}

func (h *Host) Kill() error {
	if err := h.Driver.Stop(); err != nil {
		return err
	}

	if err := h.SaveConfig(); err != nil {
		return err
	}

	return utils.WaitFor(h.MachineInState(state.Stopped))
}

func (h *Host) Restart() error {
	if h.MachineInState(state.Running)() {
		if err := h.Stop(); err != nil {
			return err
		}

		if err := utils.WaitFor(h.MachineInState(state.Stopped)); err != nil {
			return err
		}
	}

	if err := h.Start(); err != nil {
		return err
	}

	if err := utils.WaitFor(h.MachineInState(state.Running)); err != nil {
		return err
	}

	if err := h.SaveConfig(); err != nil {
		return err
	}

	return nil
}

func (h *Host) Upgrade() error {
	// TODO: refactor to provisioner
	return fmt.Errorf("centralized upgrade coming in the provisioner")
}

func (h *Host) Remove(force bool) error {
	if err := h.Driver.Remove(); err != nil {
		if !force {
			return err
		}
	}

	if err := h.SaveConfig(); err != nil {
		return err
	}

	return h.removeStorePath()
}

func (h *Host) removeStorePath() error {
	file, err := os.Stat(h.StorePath)
	if err != nil {
		return err
	}
	if !file.IsDir() {
		return fmt.Errorf("%q is not a directory", h.StorePath)
	}
	return os.RemoveAll(h.StorePath)
}

func (h *Host) GetURL() (string, error) {
	return h.Driver.GetURL()
}

func (h *Host) LoadConfig() error {
	data, err := ioutil.ReadFile(filepath.Join(h.StorePath, "config.json"))
	if err != nil {
		return err
	}

	// First pass: find the driver name and load the driver
	var hostMetadata HostMetadata
	if err := json.Unmarshal(data, &hostMetadata); err != nil {
		return err
	}

	meta := FillNestedHostMetadata(&hostMetadata)

	authOptions := meta.HostOptions.AuthOptions

	driver, err := drivers.NewDriver(hostMetadata.DriverName, h.Name, h.StorePath, authOptions.CaCertPath, authOptions.PrivateKeyPath)
	if err != nil {
		return err
	}

	h.Driver = driver

	// Second pass: unmarshal driver config into correct driver
	if err := json.Unmarshal(data, &h); err != nil {
		return err
	}

	return nil
}

func (h *Host) ConfigureAuth() error {
	provisioner, err := provision.DetectProvisioner(h.Driver)
	if err != nil {
		return err
	}

	if err := provision.ConfigureAuth(provisioner, *h.HostOptions.AuthOptions); err != nil {
		return err
	}

	return nil
}

func (h *Host) SaveConfig() error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(h.StorePath, "config.json"), data, 0600); err != nil {
		return err
	}
	return nil
}

func sshAvailableFunc(h *Host) func() bool {
	return func() bool {
		log.Debug("Getting to WaitForSSH function...")
		hostname, err := h.Driver.GetSSHHostname()
		if err != nil {
			log.Debugf("Error getting IP address waiting for SSH: %s", err)
			return false
		}
		port, err := h.Driver.GetSSHPort()
		if err != nil {
			log.Debugf("Error getting SSH port: %s", err)
			return false
		}
		if err := ssh.WaitForTCP(fmt.Sprintf("%s:%d", hostname, port)); err != nil {
			log.Debugf("Error waiting for TCP waiting for SSH: %s", err)
			return false
		}
		cmd, err := h.GetSSHCommand("exit 0")
		if err != nil {
			log.Debugf("Error getting ssh command 'exit 0' : %s", err)
			return false
		}
		if err := cmd.Run(); err != nil {
			log.Debugf("Error running ssh command 'exit 0' : %s", err)
			return false
		}
		return true
	}
}

func WaitForSSH(h *Host) error {
	if err := utils.WaitFor(sshAvailableFunc(h)); err != nil {
		return fmt.Errorf("Too many retries.  Last error: %s", err)
	}
	return nil
}
