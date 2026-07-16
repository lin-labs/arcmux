package handoff

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	launchMarkerPrefix      = "arcmux-handoff-v1:"
	launchInstructionsName  = "launch-instructions.json"
	maxLaunchInstructions   = 64 << 10
	maxLaunchRendezvous     = 4 << 10
	launchRendezvousVersion = 1
)

var ErrLaunchInstructionsUnavailable = errors.New("handoff instructions are unavailable")

type launchRendezvous struct {
	Version           int    `json:"version"`
	ProtocolStateRoot string `json:"protocol_state_root"`
}

// LaunchMarker returns the opaque target-local rendezvous token carried by a
// handoff's generic prompt. It contains no manifest fields or local paths.
func LaunchMarker(handoffID, manifestDigest string) string {
	digest := sha256.Sum256([]byte("arcmux-handoff-prompt-v1\x00" + handoffID + "\x00" + manifestDigest))
	return launchMarkerPrefix + hex.EncodeToString(digest[:])
}

// DefaultLaunchRendezvousRoot is deliberately independent of the daemon's
// config file. Agent backends may run outside the pane process and therefore
// cannot reliably inherit either its environment or alternate --config path.
func DefaultLaunchRendezvousRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "arcmux", "handoff-receive")
}

// PublishLaunchRendezvous durably binds an opaque marker to the active
// protocol state root in a fixed owner-only directory. The prompt carries
// only the marker; target-local receive uses this small index to find state
// even when the daemon was started with an alternate config.
func PublishLaunchRendezvous(rendezvousRoot, marker, protocolStateRoot string) error {
	if strings.TrimSpace(rendezvousRoot) == "" || !validLaunchMarker(marker) {
		return ErrLaunchInstructionsUnavailable
	}
	stateRoot, err := filepath.Abs(protocolStateRoot)
	if err != nil || filepath.Clean(stateRoot) != stateRoot || strings.ContainsAny(stateRoot, "\x00\r\n") {
		return ErrLaunchInstructionsUnavailable
	}
	root, err := filepath.Abs(rendezvousRoot)
	if err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	if err := ensurePrivateRoot(root); err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	root, err = canonicalPrivateDirectory(root)
	if err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	data, err := json.Marshal(launchRendezvous{Version: launchRendezvousVersion, ProtocolStateRoot: stateRoot})
	if err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	data = append(data, '\n')
	return publishPrivateFile(root, launchRendezvousFilename(marker), data)
}

// ReceiveLaunchInstructions follows the fixed owner-local rendezvous into the
// active protocol state and then applies the target-record/state/marker checks
// in ReadLaunchInstructions before returning any private content.
func ReceiveLaunchInstructions(rendezvousRoot, marker string) ([]byte, error) {
	store, err := launchRendezvousStore(rendezvousRoot, marker)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	return store.ReadLaunchInstructions(marker)
}

// AcknowledgeLaunchContext resolves only owner-local protocol state, then
// persists a marker-bound acknowledgement. It returns no launch instructions
// or other private content.
func AcknowledgeLaunchContext(rendezvousRoot, marker string, phase AcknowledgementPhase) (ContextAcknowledgement, bool, error) {
	store, err := launchRendezvousStore(rendezvousRoot, marker)
	if err != nil {
		return ContextAcknowledgement{}, false, ErrAcknowledgementUnavailable
	}
	record, replay, err := store.AcknowledgeTarget(marker, phase, time.Time{})
	if err != nil || record.ContextLoaded == nil {
		return ContextAcknowledgement{}, false, err
	}
	return *cloneAcknowledgement(record.ContextLoaded), replay, nil
}

func launchRendezvousStore(rendezvousRoot, marker string) (*Store, error) {
	if strings.TrimSpace(rendezvousRoot) == "" || !validLaunchMarker(marker) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	data, err := readPrivateRegularFile(rendezvousRoot, nil, launchRendezvousFilename(marker), maxLaunchRendezvous)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var rendezvous launchRendezvous
	if err := decoder.Decode(&rendezvous); err != nil || rendezvous.Version != launchRendezvousVersion {
		return nil, ErrLaunchInstructionsUnavailable
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	root := rendezvous.ProtocolStateRoot
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsAny(root, "\x00\r\n") {
		return nil, ErrLaunchInstructionsUnavailable
	}
	store, err := Open(root)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	return store, nil
}

// ReadLaunchInstructions resolves an opaque launch marker against durable
// target records and reads the owner-only instruction artifact without
// following symlinks. This gives agents a target-local rendezvous that does
// not depend on their tool subprocess environment inheriting arbitrary vars.
func (s *Store) ReadLaunchInstructions(marker string) ([]byte, error) {
	if !validLaunchMarker(marker) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	records, err := s.ListTarget()
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	for _, record := range records {
		if record.State != TargetLaunching && record.State != TargetLaunchWaitingAssets && record.State != TargetAccepted {
			continue
		}
		if LaunchMarker(record.Manifest.HandoffID, record.Digest) != marker {
			continue
		}
		return readPrivateRegularFile(s.root, []string{"handoff-" + record.Manifest.HandoffID}, launchInstructionsName, maxLaunchInstructions)
	}
	return nil, ErrLaunchInstructionsUnavailable
}

func validLaunchMarker(marker string) bool {
	encoded := strings.TrimPrefix(marker, launchMarkerPrefix)
	if encoded == marker || len(encoded) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size && encoded == strings.ToLower(encoded)
}

func launchRendezvousFilename(marker string) string {
	return strings.TrimPrefix(marker, launchMarkerPrefix) + ".json"
}

func publishPrivateFile(root, name string, data []byte) error {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	defer unix.Close(rootFD)
	if !securePrivateDirectoryFD(rootFD) {
		return ErrLaunchInstructionsUnavailable
	}
	var tempName string
	var file *os.File
	for attempt := 0; attempt < 16; attempt++ {
		nonce, err := randomSourceHistoryNonce()
		if err != nil {
			return ErrLaunchInstructionsUnavailable
		}
		tempName = ".receive-" + nonce + ".tmp"
		fd, err := unix.Openat(rootFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return ErrLaunchInstructionsUnavailable
		}
		file = os.NewFile(uintptr(fd), tempName)
		break
	}
	if file == nil {
		return ErrLaunchInstructionsUnavailable
	}
	defer unix.Unlinkat(rootFD, tempName, 0)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return ErrLaunchInstructionsUnavailable
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return ErrLaunchInstructionsUnavailable
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return ErrLaunchInstructionsUnavailable
	}
	if err := file.Close(); err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	if err := unix.Renameat(rootFD, tempName, rootFD, name); err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	if err := unix.Fsync(rootFD); err != nil {
		return ErrLaunchInstructionsUnavailable
	}
	return nil
}

func readPrivateRegularFile(root string, directories []string, name string, limit int64) ([]byte, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") {
		return nil, ErrLaunchInstructionsUnavailable
	}
	root, err := canonicalPrivateDirectory(root)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	defer unix.Close(rootFD)
	if !securePrivateDirectoryFD(rootFD) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	currentFD := rootFD
	openedDirs := make([]int, 0, len(directories))
	defer func() {
		for _, fd := range openedDirs {
			_ = unix.Close(fd)
		}
	}()
	for _, directory := range directories {
		if directory == "" || filepath.Base(directory) != directory || strings.ContainsAny(directory, "/\\\x00\r\n") {
			return nil, ErrLaunchInstructionsUnavailable
		}
		fd, err := unix.Openat(currentFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil || !securePrivateDirectoryFD(fd) {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			return nil, ErrLaunchInstructionsUnavailable
		}
		openedDirs = append(openedDirs, fd)
		currentFD = fd
	}
	var before unix.Stat_t
	if unix.Fstatat(currentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!safeRegularHistoryStat(before) || before.Mode&0o777 != 0o600 || before.Size <= 0 || before.Size > limit {
		return nil, ErrLaunchInstructionsUnavailable
	}
	fd, err := unix.Openat(currentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrLaunchInstructionsUnavailable
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrLaunchInstructionsUnavailable
	}
	defer file.Close()
	var opened unix.Stat_t
	if unix.Fstat(fd, &opened) != nil || !sameHistorySnapshot(before, opened) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) != before.Size {
		return nil, ErrLaunchInstructionsUnavailable
	}
	var afterFD, afterPath unix.Stat_t
	if unix.Fstat(fd, &afterFD) != nil || unix.Fstatat(currentFD, name, &afterPath, unix.AT_SYMLINK_NOFOLLOW) != nil ||
		!sameHistorySnapshot(before, afterFD) || !sameHistorySnapshot(before, afterPath) {
		return nil, ErrLaunchInstructionsUnavailable
	}
	return data, nil
}

func securePrivateDirectoryFD(fd int) bool {
	if fd < 0 {
		return false
	}
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFDIR && stat.Mode&0o077 == 0 && stat.Uid == uint32(os.Getuid())
}

func canonicalPrivateDirectory(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", ErrLaunchInstructionsUnavailable
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", ErrLaunchInstructionsUnavailable
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", ErrLaunchInstructionsUnavailable
	}
	return filepath.Clean(resolved), nil
}
