package drivers

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/go-plugins-helpers/volume"
	log "github.com/sirupsen/logrus"
)

const (
	NfsOptions   = "nfsopts"
	DefaultNfsV3 = "port=2049,nolock,proto=tcp"
)

type nfsDriver struct {
	volumeDriver
	version   int
	nfsopts   map[string]string
	nfsserver string
	share     string
	create    string
}

var (
	EmptyMap = map[string]string{}
)

func NewNFSDriver(root string, version int, nfsopts string, share string, create string, mounts *MountManager) nfsDriver {
	d := nfsDriver{
		volumeDriver: newVolumeDriver(root, mounts),
		version:      version,
		nfsopts:      map[string]string{},
		share:        share,
		create:       create,
	}

	if len(nfsopts) > 0 {
		d.nfsopts[NfsOptions] = nfsopts
	}
	return d
}

func (n nfsDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	log.Debugf("Entering Mount: %v", r)
	n.m.Lock()
	defer n.m.Unlock()

	resolvedName, resOpts := resolveName(r.Name)

	hostdir := mountpoint(n.root, resolvedName)

	var source string

	// log.Infof("n.mountm.GetOption(%s, %s): %s", resolvedName, ShareOpt, n.mountm.GetOption(resolvedName, ShareOpt))

	if n.mountm.GetOption(resolvedName, ShareOpt) == "" && !n.mountm.GetOptionAsBool(resolvedName, CreateOpt) {

		if n.mountm.mounts[resolvedName].opts == nil {
			n.mountm.mounts[resolvedName].opts = make(map[string]string)
		}

		if n.share != "" {
			n.mountm.mounts[resolvedName].opts[ShareOpt] = n.share
			source = n.fixSource(n.share)
		}

		if n.create != "" {
			n.mountm.mounts[resolvedName].opts[CreateOpt] = n.create
		}

	} else {
		source = n.fixSource(resolvedName)
	}

	// Support adhoc mounts (outside of docker volume create)
	// need to adjust source for ShareOpt
	if resOpts != nil {
		if share, found := resOpts[ShareOpt]; found {
			source = n.fixSource(share)
		}
	}

	if n.mountm.HasMount(resolvedName) {
		log.Infof("Using existing NFS volume mount: %s", hostdir)
		n.mountm.Increment(resolvedName)
		if err := run(fmt.Sprintf("grep -c %s /proc/mounts", hostdir)); err != nil {
			log.Infof("Existing NFS volume not mounted, force remount.")
			// maintain count
			if n.mountm.Count(resolvedName) > 0 {
				n.mountm.Decrement(resolvedName)
			}
		} else {
			//n.mountm.Increment(resolvedName)
			return &volume.MountResponse{Mountpoint: hostdir}, nil
		}
	}

	log.Infof("Mounting NFS volume %s on %s", source, hostdir)
	if err := createDest(hostdir); err != nil {
		if n.mountm.Count(resolvedName) > 0 {
			n.mountm.Decrement(resolvedName)
		}
		return nil, err
	}

	if n.mountm.HasMount(resolvedName) == false {
		n.mountm.Create(resolvedName, hostdir, resOpts)
	}

	n.mountm.Add(resolvedName, hostdir)

	if err := n.mountVolume(resolvedName, source, hostdir, n.version); err != nil {
		n.mountm.Decrement(resolvedName)
		return nil, err
	}

	if n.mountm.GetOption(resolvedName, ShareOpt) != "" && n.mountm.GetOptionAsBool(resolvedName, CreateOpt) {
		log.Infof("Mount: Share and Create options enabled - using %s as sub-dir mount", resolvedName)
		datavol := filepath.Join(hostdir, resolvedName)
		if err := createDest(filepath.Join(hostdir, resolvedName)); err != nil {
			n.mountm.Decrement(resolvedName)
			return nil, err
		}
		hostdir = datavol
	}

	return &volume.MountResponse{Mountpoint: hostdir}, nil
}

func (n nfsDriver) Unmount(r *volume.UnmountRequest) error {
	log.Debugf("Entering Unmount: %v", r)

	n.m.Lock()
	defer n.m.Unlock()

	resolvedName, _ := resolveName(r.Name)

	hostdir := mountpoint(n.root, resolvedName)

	if n.mountm.HasMount(resolvedName) {
		if n.mountm.Count(resolvedName) > 1 {
			log.Printf("Skipping unmount for %s - in use by other containers", resolvedName)
			n.mountm.Decrement(resolvedName)
			return nil
		}
		n.mountm.Decrement(resolvedName)
	}

	log.Infof("Unmounting volume name %s from %s", resolvedName, hostdir)

	if err := run(fmt.Sprintf("umount %s", hostdir)); err != nil {
		log.Errorf("Error unmounting volume from host: %s", err.Error())
		return err
	}

	n.mountm.DeleteIfNotManaged(resolvedName)

	// Check if directory is empty. This command will return "err" if empty
	if err := run(fmt.Sprintf("ls -1 %s | grep .", hostdir)); err == nil {
		log.Warnf("Directory %s not empty after unmount. Skipping RemoveAll call.", hostdir)
	} else {
		if err := os.RemoveAll(hostdir); err != nil {
			return err
		}
	}

	return nil
}

func (n nfsDriver) fixSource(name string) string {
	if n.mountm.HasOption(name, ShareOpt) {
		return addShareColon(n.mountm.GetOption(name, ShareOpt))
	}
	return addShareColon(name)
}

func (n nfsDriver) mountVolume(name, source, dest string, version int) error {
	var cmd string

	options := merge(n.mountm.GetOptions(name), n.nfsopts)
	opts := ""
	if val, ok := options[NfsOptions]; ok {
		opts = val
	}

	mountCmd := "mount"

	if log.GetLevel() == log.DebugLevel {
		mountCmd = mountCmd + " -v"
	}

	switch version {
	case 3:
		log.Debugf("Mounting with NFSv3 - src: %s, dest: %s", source, dest)
		if len(opts) < 1 {
			opts = DefaultNfsV3
		}
		cmd = fmt.Sprintf("%s -t nfs -o %s %s %s", mountCmd, opts, source, dest)
	default:
		log.Infof("Mounting with NFSv4 - opts: %s, src: %s, dest: %s", opts, source, dest)
		if len(opts) > 0 {
			cmd = fmt.Sprintf("%s -t nfs4 -o %s %s %s", mountCmd, opts, source, dest)
		} else {
			cmd = fmt.Sprintf("%s -t nfs4 %s %s", mountCmd, source, dest)
		}
	}
	log.Infof("exec: %s\n", cmd)
	return run(cmd)
}
