package drivers

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
)

// XTables is an implmentation of LXD firewall using {ip, ip6, eb}tables
type XTables struct{}

//networkIPTablesComment returns the iptables comment that is added to each network related rule.
func (d XTables) networkIPTablesComment(networkName string) string {
	return fmt.Sprintf("LXD network %s", networkName)
}

// NetworkSetupForwardingPolicy allows forwarding dependent on boolean argument
func (d XTables) NetworkSetupForwardingPolicy(networkName string, ipVersion uint, allow bool) error {
	forwardType := "REJECT"
	if allow {
		forwardType = "ACCEPT"
	}

	comment := d.networkIPTablesComment(networkName)
	err := d.iptablesPrepend(ipVersion, comment, "filter", "FORWARD", "-i", networkName, "-j", forwardType)
	if err != nil {
		return err
	}

	err = d.iptablesPrepend(ipVersion, comment, "filter", "FORWARD", "-o", networkName, "-j", forwardType)

	if err != nil {
		return err
	}

	return nil
}

// NetworkSetupOutboundNAT configures outbound NAT.
// If srcIP is non-nil then SNAT is used with the specified address, otherwise MASQUERADE mode is used.
func (d XTables) NetworkSetupOutboundNAT(networkName string, subnet *net.IPNet, srcIP net.IP, appendRule bool) error {
	family := uint(4)
	if subnet.IP.To4() == nil {
		family = 6
	}

	args := []string{
		"-s", subnet.String(),
		"!", "-d", subnet.String(),
	}

	// If SNAT IP not supplied then use the IP of the outbound interface (MASQUERADE).
	if srcIP == nil {
		args = append(args, "-j", "MASQUERADE")
	} else {
		args = append(args, "-j", "SNAT", "--to", srcIP.String())
	}

	comment := d.networkIPTablesComment(networkName)

	if appendRule {
		err := d.iptablesAppend(family, comment, "nat", "POSTROUTING", args...)
		if err != nil {
			return err
		}

	} else {
		err := d.iptablesPrepend(family, comment, "nat", "POSTROUTING", args...)
		if err != nil {
			return err
		}
	}

	return nil
}

// NetworkSetupDHCPDNSAccess sets up basic iptables overrides for DHCP/DNS.
func (d XTables) NetworkSetupDHCPDNSAccess(networkName string, ipVersion uint) error {
	var rules [][]string
	if ipVersion == 4 {
		rules = [][]string{
			{"4", networkName, "filter", "INPUT", "-i", networkName, "-p", "udp", "--dport", "67", "-j", "ACCEPT"},
			{"4", networkName, "filter", "INPUT", "-i", networkName, "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
			{"4", networkName, "filter", "INPUT", "-i", networkName, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"},
			{"4", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "udp", "--sport", "67", "-j", "ACCEPT"},
			{"4", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "udp", "--sport", "53", "-j", "ACCEPT"},
			{"4", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "tcp", "--sport", "53", "-j", "ACCEPT"}}
	} else if ipVersion == 6 {
		rules = [][]string{
			{"6", networkName, "filter", "INPUT", "-i", networkName, "-p", "udp", "--dport", "547", "-j", "ACCEPT"},
			{"6", networkName, "filter", "INPUT", "-i", networkName, "-p", "udp", "--dport", "53", "-j", "ACCEPT"},
			{"6", networkName, "filter", "INPUT", "-i", networkName, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"},
			{"6", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "udp", "--sport", "547", "-j", "ACCEPT"},
			{"6", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "udp", "--sport", "53", "-j", "ACCEPT"},
			{"6", networkName, "filter", "OUTPUT", "-o", networkName, "-p", "tcp", "--sport", "53", "-j", "ACCEPT"}}
	} else {
		return fmt.Errorf("Invalid IP version")
	}

	comment := d.networkIPTablesComment(networkName)

	for _, rule := range rules {
		ipVersion, err := strconv.ParseUint(rule[0], 10, 0)
		if err != nil {
			return err
		}

		err = d.iptablesPrepend(uint(ipVersion), comment, rule[2], rule[3], rule[4:]...)
		if err != nil {
			return err
		}
	}

	return nil
}

// NetworkSetupDHCPv4Checksum attempts a workaround for broken DHCP clients.
func (d XTables) NetworkSetupDHCPv4Checksum(networkName string) error {
	comment := d.networkIPTablesComment(networkName)
	return d.iptablesPrepend(4, comment, "mangle", "POSTROUTING", "-o", networkName, "-p", "udp", "--dport", "68", "-j", "CHECKSUM", "--checksum-fill")
}

// NetworkClear removes network rules from filter, mangle and nat tables.
func (d XTables) NetworkClear(networkName string, ipVersion uint) error {
	err := d.iptablesClear(ipVersion, d.networkIPTablesComment(networkName), "filter", "mangle", "nat")
	if err != nil {
		return err
	}

	return nil
}

//instanceDeviceIPTablesComment returns the iptables comment that is added to each instance device related rule.
func (d XTables) instanceDeviceIPTablesComment(projectName, instanceName, deviceName string) string {
	return fmt.Sprintf("LXD container %s (%s)", project.Prefix(projectName, instanceName), deviceName)
}

// InstanceSetupBridgeFilter sets up the filter rules to apply bridged device IP filtering.
func (d XTables) InstanceSetupBridgeFilter(projectName, instanceName, deviceName, parentName, hostName, hwAddr string, IPv4, IPv6 net.IP) error {
	comment := d.instanceDeviceIPTablesComment(projectName, instanceName, deviceName)

	rules := d.generateFilterEbtablesRules(hostName, hwAddr, IPv4, IPv6)
	for _, rule := range rules {
		_, err := shared.RunCommand(rule[0], append([]string{"--concurrent"}, rule[1:]...)...)
		if err != nil {
			return err
		}
	}

	rules, err := d.generateFilterIptablesRules(projectName, instanceName, parentName, hostName, hwAddr, IPv4, IPv6)
	if err != nil {
		return err
	}

	for _, rule := range rules {
		ipVersion, err := strconv.ParseUint(rule[0], 10, 0)
		if err != nil {
			return err
		}

		err = d.iptablesPrepend(uint(ipVersion), comment, "filter", rule[1], rule[2:]...)
		if err != nil {
			return err
		}
	}

	return nil
}

// InstanceClearBridgeFilter removes any filter rules that were added to apply bridged device IP filtering.
func (d XTables) InstanceClearBridgeFilter(projectName, instanceName, deviceName, parentName, hostName, hwAddr string, IPv4, IPv6 net.IP) error {
	comment := d.instanceDeviceIPTablesComment(projectName, instanceName, deviceName)

	// Get a current list of rules active on the host.
	out, err := shared.RunCommand("ebtables", "--concurrent", "-L", "--Lmac2", "--Lx")
	if err != nil {
		return fmt.Errorf("Failed to get a list of network filters to for %q: %v", deviceName, err)
	}

	// Get a list of rules that we would have applied on instance start.
	rules := d.generateFilterEbtablesRules(hostName, hwAddr, IPv4, IPv6)

	errs := []error{}
	// Iterate through each active rule on the host and try and match it to one the LXD rules.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		fieldsLen := len(fields)

		for _, rule := range rules {
			// Rule doesn't match if the field lenths aren't the same, move on.
			if len(rule) != fieldsLen {
				continue
			}

			// Check whether active rule matches one of our rules to delete.
			if !d.matchEbtablesRule(fields, rule, true) {
				continue
			}

			// If we get this far, then the current host rule matches one of our LXD
			// rules, so we should run the modified command to delete it.
			_, err = shared.RunCommand(fields[0], append([]string{"--concurrent"}, fields[1:]...)...)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Remove any ip6tables rules added as part of bridge filtering.
	err = d.iptablesClear(6, comment, "filter")
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("Failed to remove network filters rule for %q: %v", deviceName, errs)
	}

	return nil
}

// InstanceSetupProxyNAT creates DNAT rules for proxy devices.
func (d XTables) InstanceSetupProxyNAT(projectName, instanceName, deviceName string, listen, connect *deviceConfig.ProxyAddress) error {
	connectAddrCount := len(connect.Addr)
	comment := d.instanceDeviceIPTablesComment(projectName, instanceName, deviceName)

	if connectAddrCount < 1 {
		return fmt.Errorf("At least 1 connect address must be supplied")
	}

	if connectAddrCount > 1 && len(listen.Addr) != connectAddrCount {
		return fmt.Errorf("More than 1 connect addresses have been supplied, but insufficient for listen addresses")
	}

	revert := revert.New()
	defer revert.Fail()
	revert.Add(func() { d.InstanceClearProxyNAT(projectName, instanceName, deviceName) })

	for i, lAddr := range listen.Addr {
		listenHost, listenPort, err := net.SplitHostPort(lAddr)
		if err != nil {
			return err
		}

		// Use the connect address that corresponds to the listen address (unless only 1 is specified).
		connectIndex := 0
		if connectAddrCount > 1 {
			connectIndex = i
		}

		connectHost, connectPort, err := net.SplitHostPort(connect.Addr[connectIndex])
		if err != nil {
			return err
		}

		// Figure out if we are using iptables or ip6tables and format the destination host/port as appropriate.
		ipVersion := uint(4)
		toDest := fmt.Sprintf("%s:%s", connectHost, connectPort)
		connectIP := net.ParseIP(connectHost)
		if connectIP.To4() == nil {
			ipVersion = 6
			toDest = fmt.Sprintf("[%s]:%s", connectHost, connectPort)
		}

		// outbound <-> container.
		err = d.iptablesPrepend(ipVersion, comment, "nat", "PREROUTING", "-p", listen.ConnType, "--destination", listenHost, "--dport", listenPort, "-j", "DNAT", "--to-destination", toDest)
		if err != nil {
			return err
		}

		// host <-> container.
		err = d.iptablesPrepend(ipVersion, comment, "nat", "OUTPUT", "-p", listen.ConnType, "--destination", listenHost, "--dport", listenPort, "-j", "DNAT", "--to-destination", toDest)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

// InstanceClearProxyNAT remove DNAT rules for proxy devices.
func (d XTables) InstanceClearProxyNAT(projectName, instanceName, deviceName string) error {
	comment := d.instanceDeviceIPTablesComment(projectName, instanceName, deviceName)
	errs := []error{}
	err := d.iptablesClear(4, comment, "nat")
	if err != nil {
		errs = append(errs, err)
	}

	err = d.iptablesClear(6, comment, "nat")
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return err
	}

	return nil
}

// generateFilterEbtablesRules returns a customised set of ebtables filter rules based on the device.
func (d XTables) generateFilterEbtablesRules(hostName, hwAddr string, IPv4, IPv6 net.IP) [][]string {
	// MAC source filtering rules. Blocks any packet coming from instance with an incorrect Ethernet source MAC.
	// This is required for IP filtering too.
	rules := [][]string{
		{"ebtables", "-t", "filter", "-A", "INPUT", "-s", "!", hwAddr, "-i", hostName, "-j", "DROP"},
		{"ebtables", "-t", "filter", "-A", "FORWARD", "-s", "!", hwAddr, "-i", hostName, "-j", "DROP"},
	}

	if IPv4 != nil {
		rules = append(rules,
			// Prevent ARP MAC spoofing (prevents the instance poisoning the ARP cache of its neighbours with a MAC address that isn't its own).
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "ARP", "-i", hostName, "--arp-mac-src", "!", hwAddr, "-j", "DROP"},
			[]string{"ebtables", "-t", "filter", "-A", "FORWARD", "-p", "ARP", "-i", hostName, "--arp-mac-src", "!", hwAddr, "-j", "DROP"},
			// Prevent ARP IP spoofing (prevents the instance redirecting traffic for IPs that are not its own).
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "ARP", "-i", hostName, "--arp-ip-src", "!", IPv4.String(), "-j", "DROP"},
			[]string{"ebtables", "-t", "filter", "-A", "FORWARD", "-p", "ARP", "-i", hostName, "--arp-ip-src", "!", IPv4.String(), "-j", "DROP"},
			// Allow DHCPv4 to the host only. This must come before the IP source filtering rules below.
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "IPv4", "-s", hwAddr, "-i", hostName, "--ip-src", "0.0.0.0", "--ip-dst", "255.255.255.255", "--ip-proto", "udp", "--ip-dport", "67", "-j", "ACCEPT"},
			// IP source filtering rules. Blocks any packet coming from instance with an incorrect IP source address.
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "IPv4", "-i", hostName, "--ip-src", "!", IPv4.String(), "-j", "DROP"},
			[]string{"ebtables", "-t", "filter", "-A", "FORWARD", "-p", "IPv4", "-i", hostName, "--ip-src", "!", IPv4.String(), "-j", "DROP"},
		)
	}

	if IPv6 != nil {
		rules = append(rules,
			// Allow DHCPv6 and Router Solicitation to the host only. This must come before the IP source filtering rules below.
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "IPv6", "-s", hwAddr, "-i", hostName, "--ip6-src", "fe80::/ffc0::", "--ip6-dst", "ff02::1:2/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "--ip6-proto", "udp", "--ip6-dport", "547", "-j", "ACCEPT"},
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "IPv6", "-s", hwAddr, "-i", hostName, "--ip6-src", "fe80::/ffc0::", "--ip6-dst", "ff02::2/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "--ip6-proto", "ipv6-icmp", "--ip6-icmp-type", "router-solicitation", "-j", "ACCEPT"},
			// IP source filtering rules. Blocks any packet coming from instance with an incorrect IP source address.
			[]string{"ebtables", "-t", "filter", "-A", "INPUT", "-p", "IPv6", "-i", hostName, "--ip6-src", "!", fmt.Sprintf("%s/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", IPv6.String()), "-j", "DROP"},
			[]string{"ebtables", "-t", "filter", "-A", "FORWARD", "-p", "IPv6", "-i", hostName, "--ip6-src", "!", fmt.Sprintf("%s/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", IPv6.String()), "-j", "DROP"},
		)
	}

	return rules
}

// generateFilterIptablesRules returns a customised set of iptables filter rules based on the device.
func (d XTables) generateFilterIptablesRules(projectName, instanceName, parentName, hostName, hwAddr string, _ net.IP, IPv6 net.IP) (rules [][]string, err error) {
	mac, err := net.ParseMAC(hwAddr)
	if err != nil {
		return
	}

	macHex := hex.EncodeToString(mac)

	// These rules below are implemented using ip6tables because the functionality to inspect
	// the contents of an ICMPv6 packet does not exist in ebtables (unlike for IPv4 ARP).
	// Additionally, ip6tables doesn't really provide a nice way to do what we need here, so we
	// have resorted to doing a raw hex comparison of the packet contents at fixed positions.
	// If these rules are not added then it is possible to hijack traffic for another IP that is
	// not assigned to the instance by sending a specially crafted gratuitous NDP packet with
	// correct source address and MAC at the IP & ethernet layers, but a fraudulent IP or MAC
	// inside the ICMPv6 NDP packet.
	if IPv6 != nil {
		ipv6Hex := hex.EncodeToString(IPv6)
		rules = append(rules,
			// Prevent Neighbor Advertisement IP spoofing (prevents the instance redirecting traffic for IPs that are not its own).
			[]string{"6", "INPUT", "-i", parentName, "-p", "ipv6-icmp", "-m", "physdev", "--physdev-in", hostName, "-m", "icmp6", "--icmpv6-type", "136", "-m", "string", "!", "--hex-string", fmt.Sprintf("|%s|", ipv6Hex), "--algo", "bm", "--from", "48", "--to", "64", "-j", "DROP"},
			[]string{"6", "FORWARD", "-i", parentName, "-p", "ipv6-icmp", "-m", "physdev", "--physdev-in", hostName, "-m", "icmp6", "--icmpv6-type", "136", "-m", "string", "!", "--hex-string", fmt.Sprintf("|%s|", ipv6Hex), "--algo", "bm", "--from", "48", "--to", "64", "-j", "DROP"},
			// Prevent Neighbor Advertisement MAC spoofing (prevents the instance poisoning the NDP cache of its neighbours with a MAC address that isn't its own).
			[]string{"6", "INPUT", "-i", parentName, "-p", "ipv6-icmp", "-m", "physdev", "--physdev-in", hostName, "-m", "icmp6", "--icmpv6-type", "136", "-m", "string", "!", "--hex-string", fmt.Sprintf("|%s|", macHex), "--algo", "bm", "--from", "66", "--to", "72", "-j", "DROP"},
			[]string{"6", "FORWARD", "-i", parentName, "-p", "ipv6-icmp", "-m", "physdev", "--physdev-in", hostName, "-m", "icmp6", "--icmpv6-type", "136", "-m", "string", "!", "--hex-string", fmt.Sprintf("|%s|", macHex), "--algo", "bm", "--from", "66", "--to", "72", "-j", "DROP"},
		)
	}

	return
}

// matchEbtablesRule compares an active rule to a supplied match rule to see if they match.
// If deleteMode is true then the "-A" flag in the active rule will be modified to "-D" and will
// not be part of the equality match. This allows delete commands to be generated from dumped add commands.
func (d XTables) matchEbtablesRule(activeRule []string, matchRule []string, deleteMode bool) bool {
	for i := range matchRule {
		// Active rules will be dumped in "add" format, we need to detect
		// this and switch it to "delete" mode if requested. If this has already been
		// done then move on, as we don't want to break the comparison below.
		if deleteMode && (activeRule[i] == "-A" || activeRule[i] == "-D") {
			activeRule[i] = "-D"
			continue
		}

		// Mangle to line up with different versions of ebtables.
		active := strings.Replace(activeRule[i], "/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "", -1)
		match := strings.Replace(matchRule[i], "/ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", "", -1)
		active = strings.Replace(active, "fe80::/ffc0::", "fe80::/10", -1)
		match = strings.Replace(match, "fe80::/ffc0::", "fe80::/10", -1)

		// Check the match rule field matches the active rule field.
		// If they don't match, then this isn't one of our rules.
		if active != match {
			return false
		}
	}

	return true
}

// iptablesAdd adds an iptables rule.
func (d XTables) iptablesConfig(ipVersion uint, comment, table, method, chain string, rule ...string) error {
	var cmd string
	if ipVersion == 4 {
		cmd = "iptables"
	} else if ipVersion == 6 {
		cmd = "ip6tables"
	} else {
		return fmt.Errorf("Invalid IP version")
	}

	_, err := exec.LookPath(cmd)
	if err != nil {
		return fmt.Errorf("Asked to setup IPv%d firewalling but %s can't be found", ipVersion, cmd)
	}

	baseArgs := []string{"-w", "-t", table}

	// Check for an existing entry
	args := append(baseArgs, []string{"-C", chain}...)
	args = append(args, rule...)
	args = append(args, "-m", "comment", "--comment", fmt.Sprintf("generated for %s", comment))
	_, err = shared.RunCommand(cmd, args...)
	if err == nil {
		return nil
	}

	args = append(baseArgs, []string{method, chain}...)
	args = append(args, rule...)
	args = append(args, "-m", "comment", "--comment", fmt.Sprintf("generated for %s", comment))

	_, err = shared.TryRunCommand(cmd, args...)
	if err != nil {
		return err
	}

	return nil
}

// iptablesAppend appends an iptables rule.
func (d XTables) iptablesAppend(ipVersion uint, comment, table, chain string, rule ...string) error {
	return d.iptablesConfig(ipVersion, comment, table, "-A", chain, rule...)
}

// iptablesPrepend prepends an iptables rule.
func (d XTables) iptablesPrepend(ipVersion uint, comment, table, chain string, rule ...string) error {
	return d.iptablesConfig(ipVersion, comment, table, "-I", chain, rule...)
}

// iptablesClear clears iptables rules matching the supplied comment in the specified tables.
func (d XTables) iptablesClear(ipVersion uint, comment string, fromTables ...string) error {
	var cmd string
	var tablesFile string
	if ipVersion == 4 {
		cmd = "iptables"
		tablesFile = "/proc/self/net/ip_tables_names"
	} else if ipVersion == 6 {
		cmd = "ip6tables"
		tablesFile = "/proc/self/net/ip6_tables_names"
	} else {
		return fmt.Errorf("Invalid IP version")
	}

	// Detect kernels that lack IPv6 support.
	if !shared.PathExists("/proc/sys/net/ipv6") && ipVersion == 6 {
		return nil
	}

	// Check command exists.
	_, err := exec.LookPath(cmd)
	if err != nil {
		return nil
	}

	// Check which tables exist.
	var tables []string // Uninitialised slice indicates we haven't opened the table file yet.
	file, err := os.Open(tablesFile)
	if err != nil {
		logger.Warnf("Failed getting list of tables from %q, assuming all requested tables exist", tablesFile)
	} else {
		tables = []string{} // Initialise the tables slice indcating we were able to open the tables file.
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			tables = append(tables, scanner.Text())
		}
		file.Close()
	}

	for _, fromTable := range fromTables {
		if tables != nil && !shared.StringInSlice(fromTable, tables) {
			// If we successfully opened the tables file, and the requested table is not present,
			// then skip trying to get a list of rules from that table.
			continue
		}

		baseArgs := []string{"-w", "-t", fromTable}
		// List the rules.
		args := append(baseArgs, "-S")
		output, err := shared.TryRunCommand(cmd, args...)
		if err != nil {
			return fmt.Errorf("Failed to list IPv%d rules for %s (table %s)", ipVersion, comment, fromTable)
		}

		for _, line := range strings.Split(output, "\n") {
			if !strings.Contains(line, fmt.Sprintf("generated for %s", comment)) {
				continue
			}

			// Remove the entry.
			fields := strings.Fields(line)
			fields[0] = "-D"

			args = append(baseArgs, fields...)
			_, err = shared.TryRunCommand("sh", "-c", fmt.Sprintf("%s %s", cmd, strings.Join(args, " ")))
			if err != nil {
				return err
			}
		}
	}

	return nil
}
