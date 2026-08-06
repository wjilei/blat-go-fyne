// Package serial enumerates the COM/serial ports visible to the GUI on
// the current OS. The rest of the project talks only to ListPorts; the
// port-opening / read-write path lives behind the device drivers so
// that the GUI layer stays free of any open()/Close() lifecycle.
//
// Implementation notes
//
//   - We deliberately avoid third-party libraries (go.bug.st/serial and
//     friends) so the project stays cgo-free and cold builds stay fast.
//   - On Windows we read the well-known registry key
//     HKLM\HARDWARE\DEVICEMAP\SERIALCOMM. Keys are kernel device paths
//     ("\Device\Serial0"), values are the "COMx" names that
//     user-mode code (and our device driver) actually opens.
//   - The set is stable for the lifetime of the process; a hot-plug
//     event will not be reflected until the next ListPorts() call.
//   - We dedupe and sort the result so a UI dropdown never shows the
//     same COM port twice (the same physical port can show up under
//     several device paths in some virtualisation stacks).
package serial

import (
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// ListPorts returns the names of every serial port Windows currently
// reports. On Windows this is the values under
// HKLM\HARDWARE\DEVICEMAP\SERIALCOMM (e.g. "COM1", "COM3"). The slice
// is sorted in ascending COM-number order so a dropdown is stable.
//
// A nil error with a non-empty slice is the happy path. A nil slice
// with a nil error means the key was missing (no serial ports at all),
// which we treat as "none available" rather than an error condition.
func ListPorts() ([]string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.READ)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil, nil
		}
		return nil, err
	}
	defer k.Close()

	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(names))
	var ports []string
	for _, n := range names {
		v, _, _ := k.GetStringValue(n)
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		ports = append(ports, v)
	}
	sort.Slice(ports, func(i, j int) bool {
		// Sort COM ports numerically: "COM2" < "COM10" (lexicographic
		// sort would put "COM10" before "COM2").
		return comLess(ports[i], ports[j])
	})
	return ports, nil
}

// comLess compares two "COM<n>" names by their numeric suffix. Names
// that don't match the pattern fall back to lexicographic order.
func comLess(a, b string) bool {
	na, oka := comNum(a)
	nb, okb := comNum(b)
	if oka && okb {
		return na < nb
	}
	return a < b
}

// comNum returns the trailing integer of a "COM<n>" name and whether
// the prefix matched. The case is fixed (registry stores "COM" upper
// case) but we accept any case to be friendly to other data sources.
func comNum(s string) (int, bool) {
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "COM") {
		return 0, false
	}
	rest := s[3:]
	if rest == "" {
		return 0, false
	}
	n := 0
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
