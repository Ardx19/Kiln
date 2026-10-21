package sysinfo

import "golang.org/x/sys/unix"

// remoteFilesystems maps statfs magic numbers to names, for filesystems
// whose files live on another machine or on the VM host. Every stat goes
// over the wire, and mtimes come from the other side's clock.
var remoteFilesystems = map[int64]string{
	0x6969:     "NFS",
	0x01021997: "9p (WSL's /mnt drives)",
	0x6a656a63: "virtiofs",
	0xff534d42: "CIFS",
	0xfe534d42: "SMB",
}

// RemoteFilesystem returns the name of the network or VM-shared
// filesystem dir is on, or "" if it's on anything else.
func RemoteFilesystem(dir string) string {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return ""
	}
	return remoteFilesystems[int64(st.Type)]
}
