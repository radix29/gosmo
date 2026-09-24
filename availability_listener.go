package gosmo

// availability_listener.go is the listener clients connect through: the
// listeners and their IP addresses as read from the group, and the
// ADD/MODIFY/REMOVE LISTENER statements that change them. The role each
// statement must be run from is availability_group.go's § Operations preamble.

import (
	"context"
	"fmt"
	"strings"
)

// -- Listeners -----------------------------------------------------------------

// AvailabilityGroupListener is one virtual network name clients connect to.
// Its addresses are carried inline rather than behind another round trip,
// since a listener without them tells a caller almost nothing.
type AvailabilityGroupListener struct {
	GroupID    string
	ListenerID string
	DNSName    string
	Port       int

	// IsConformant is false for a listener created outside SQL Server (added
	// directly in the cluster manager), whose configuration SQL Server can
	// report but not fully validate.
	IsConformant bool

	IPConfigurationString    string
	IsDistributedNetworkName bool

	IPAddresses []AvailabilityListenerIP
}

// AvailabilityListenerIP is one address bound to a listener. A multi-subnet
// availability group has one per subnet.
type AvailabilityListenerIP struct {
	IPAddress  string
	SubnetMask string
	IsDHCP     bool
	State      string
}

// listenerSelect builds the sys.availability_group_listeners read for the
// connected server's version. is_distributed_network_name arrived in SQL
// Server 2019, and ISNULL does not save a name the parser cannot resolve: on
// 2016 or 2017 the unguarded column fails every listener read with "Invalid
// column name". Dated from
// https://learn.microsoft.com/sql/relational-databases/system-catalog-views/sys-availability-group-listeners-transact-sql.
func (s *Server) listenerSelect() string {
	major := s.serverMajorVersion()
	return `
	SELECT CONVERT(varchar(36), l.group_id), CONVERT(varchar(36), l.listener_id),
	       ISNULL(l.dns_name,''), ISNULL(l.port, 0), ISNULL(l.is_conformant, 0),
	       ISNULL(l.ip_configuration_string_from_cluster,''),
	       ` + colSince(major, SQLServer2019, "ISNULL(l.is_distributed_network_name, 0)", "CAST(0 AS bit)") + `
	FROM sys.availability_group_listeners l
	WHERE l.group_id = @p1
	ORDER BY l.dns_name`
}

// Listeners returns the group's listeners, each with its IP addresses.
func (ag *AvailabilityGroup) Listeners(ctx context.Context) ([]*AvailabilityGroupListener, error) {
	s := ag.server

	rows, err := s.query(ctx, s.listenerSelect(), ag.ID)
	if err != nil {
		return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	var listeners []*AvailabilityGroupListener
	byID := map[string]*AvailabilityGroupListener{}
	for rows.Next() {
		l := &AvailabilityGroupListener{}
		if err := rows.Scan(
			&l.GroupID, &l.ListenerID, &l.DNSName, &l.Port, &l.IsConformant,
			&l.IPConfigurationString, &l.IsDistributedNetworkName,
		); err != nil {
			return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
		}
		listeners = append(listeners, l)
		byID[strings.ToLower(l.ListenerID)] = l
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("gosmo: list listeners of availability group %q: %w", ag.Name, err)
	}
	if len(listeners) == 0 {
		return nil, nil
	}

	if err := ag.attachListenerIPs(ctx, byID); err != nil {
		return nil, err
	}
	return listeners, nil
}

// attachListenerIPs fills in each listener's IPAddresses. Split out so the
// listener scan above closes its rows before this second query runs.
func (ag *AvailabilityGroup) attachListenerIPs(ctx context.Context, byID map[string]*AvailabilityGroupListener) error {
	const q = `
	SELECT CONVERT(varchar(36), ip.listener_id),
	       ISNULL(ip.ip_address,''), ISNULL(ip.ip_subnet_mask,''),
	       ISNULL(ip.is_dhcp, 0), ISNULL(ip.state_desc,'')
	FROM sys.availability_group_listener_ip_addresses ip
	JOIN sys.availability_group_listeners l ON l.listener_id = ip.listener_id
	WHERE l.group_id = @p1`

	rows, err := ag.server.query(ctx, q, ag.ID)
	if err != nil {
		return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var listenerID string
		var ip AvailabilityListenerIP
		if err := rows.Scan(&listenerID, &ip.IPAddress, &ip.SubnetMask, &ip.IsDHCP, &ip.State); err != nil {
			return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
		}
		if l := byID[strings.ToLower(listenerID)]; l != nil {
			l.IPAddresses = append(l.IPAddresses, ip)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gosmo: list listener addresses of availability group %q: %w", ag.Name, err)
	}
	return nil
}

// -- Listener operations -------------------------------------------------------------

// AvailabilityListenerSpec describes a listener to create with AddListener.
//
// Exactly one addressing mode has to be chosen: either DHCP, or one or more
// static IPAddresses. A group can have only one listener at a time, so adding a
// second is an error (19477) rather than a second name to reach it by.
type AvailabilityListenerSpec struct {
	// DNSName is the virtual network name clients connect to. Required.
	DNSName string

	// Port is the TCP port the listener answers on. Zero omits PORT from the
	// statement, which SQL Server defaults to 1433.
	Port int

	// IPAddresses are the static addresses to bind, one per subnet. An entry
	// with an empty SubnetMask is emitted as an IPv6 address, which takes no
	// mask; an IPv4 entry needs one.
	IPAddresses []AvailabilityListenerIPSpec

	// DHCP requests an address from DHCP instead of binding static ones.
	// Mutually exclusive with IPAddresses.
	DHCP bool

	// DHCPSubnet and DHCPSubnetMask optionally name the subnet to take the
	// address from — WITH DHCP ON (N'network', N'mask'). Both or neither.
	DHCPSubnet     string
	DHCPSubnetMask string
}

// AvailabilityListenerIPSpec is one static address for a listener being created.
type AvailabilityListenerIPSpec struct {
	IPAddress string

	// SubnetMask is required for IPv4 and must be empty for IPv6.
	SubnetMask string
}

// addListenerClause builds the ADD LISTENER clause, validating the spec.
//
// PORT is emitted for both addressing modes. The documented grammar allows it
// only after WITH IP, but SQL Server 2025 parses "WITH DHCP, PORT = n" too, and
// leaving it off a DHCP listener would silently give it 1433.
func (spec AvailabilityListenerSpec) addListenerClause() (string, error) {
	if strings.TrimSpace(spec.DNSName) == "" {
		return "", fmt.Errorf("listener has no DNS name")
	}
	if spec.Port < 0 || spec.Port > 65535 {
		return "", fmt.Errorf("listener port %d out of range 1-65535", spec.Port)
	}
	if spec.DHCP && len(spec.IPAddresses) > 0 {
		return "", fmt.Errorf("listener %q asks for both DHCP and static addresses", spec.DNSName)
	}
	if !spec.DHCP && len(spec.IPAddresses) == 0 {
		return "", fmt.Errorf("listener %q has neither DHCP nor a static address", spec.DNSName)
	}

	var with string
	if spec.DHCP {
		with = "WITH DHCP"
		switch {
		case spec.DHCPSubnet != "" && spec.DHCPSubnetMask != "":
			with += fmt.Sprintf(" ON (%s, %s)", QuoteLiteral(spec.DHCPSubnet), QuoteLiteral(spec.DHCPSubnetMask))
		case spec.DHCPSubnet != "" || spec.DHCPSubnetMask != "":
			return "", fmt.Errorf("listener %q gives only one half of the DHCP subnet and mask", spec.DNSName)
		}
	} else {
		addrs := make([]string, 0, len(spec.IPAddresses))
		for _, ip := range spec.IPAddresses {
			addr, err := listenerIPLiteral(ip)
			if err != nil {
				return "", fmt.Errorf("listener %q has an empty IP address", spec.DNSName)
			}
			addrs = append(addrs, addr)
		}
		with = "WITH IP (" + strings.Join(addrs, ", ") + ")"
	}
	if spec.Port > 0 {
		with += fmt.Sprintf(", PORT = %d", spec.Port)
	}
	return fmt.Sprintf("ADD LISTENER %s (%s)", QuoteLiteral(spec.DNSName), with), nil
}

// AddListener creates the group's listener. Run against the primary.
//
// Under an EXTERNAL cluster type this records the listener in SQL Server's own
// metadata only — the address itself belongs to the external cluster manager
// (on Linux, a Pacemaker IPaddr2 resource), which has to be configured
// separately for clients to actually reach it. Unlike failover, the statement
// itself is accepted.
func (ag *AvailabilityGroup) AddListener(ctx context.Context, spec AvailabilityListenerSpec) error {
	clause, err := spec.addListenerClause()
	if err != nil {
		return fmt.Errorf("gosmo: add listener to availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: add listener %q to availability group %q: %w", spec.DNSName, ag.Name, err)
	}
	return nil
}

// modifyListenerClause builds a MODIFY LISTENER clause around one option.
//
// The grammar takes exactly one option per statement — a single MODIFY LISTENER
// cannot both change the port and add an address — so each caller below builds
// its own statement rather than accumulating a list.
func modifyListenerClause(dnsName, option string) (string, error) {
	if strings.TrimSpace(dnsName) == "" {
		return "", fmt.Errorf("listener has no DNS name")
	}
	return fmt.Sprintf("MODIFY LISTENER %s (%s)", QuoteLiteral(dnsName), option), nil
}

// SetListenerPort changes the port an existing listener answers on. Run against
// the primary.
//
// Clients already connected through the old port stay connected; only new
// connections are affected, and any that name the port explicitly will need
// updating.
func (ag *AvailabilityGroup) SetListenerPort(ctx context.Context, dnsName string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("gosmo: modify listener %q on availability group %q: port %d out of range 1-65535", dnsName, ag.Name, port)
	}
	clause, err := modifyListenerClause(dnsName, fmt.Sprintf("PORT = %d", port))
	if err != nil {
		return fmt.Errorf("gosmo: modify listener on availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: set listener %q port on availability group %q: %w", dnsName, ag.Name, err)
	}
	return nil
}

// AddListenerIP binds another static address to an existing listener — the
// second and later subnets of a multi-subnet listener. Run against the primary.
//
// A listener created WITH DHCP cannot be given static addresses this way; SQL
// Server rejects the statement.
//
// There is no matching REMOVE IP: an address bound here stays for the life of
// the listener, and correcting one means REMOVE LISTENER and ADD LISTENER.
//
// Under an EXTERNAL cluster type the address is recorded but not brought up —
// it appears in sys.availability_group_listener_ip_addresses as OFFLINE,
// because the external cluster manager owns the address, not SQL Server.
// Verified on SQL Server 2025 under Pacemaker.
func (ag *AvailabilityGroup) AddListenerIP(ctx context.Context, dnsName string, ip AvailabilityListenerIPSpec) error {
	addr, err := listenerIPLiteral(ip)
	if err != nil {
		return fmt.Errorf("gosmo: add an address to listener %q on availability group %q: %w", dnsName, ag.Name, err)
	}
	clause, err := modifyListenerClause(dnsName, "ADD IP "+addr)
	if err != nil {
		return fmt.Errorf("gosmo: modify listener on availability group %q: %w", ag.Name, err)
	}
	if err := ag.alter(ctx, clause); err != nil {
		return fmt.Errorf("gosmo: add address %q to listener %q on availability group %q: %w", ip.IPAddress, dnsName, ag.Name, err)
	}
	return nil
}

// listenerIPLiteral renders one address as the grammar's parenthesised pair,
// or single value for IPv6, which takes no mask.
func listenerIPLiteral(ip AvailabilityListenerIPSpec) (string, error) {
	if strings.TrimSpace(ip.IPAddress) == "" {
		return "", fmt.Errorf("empty IP address")
	}
	if ip.SubnetMask == "" {
		return "(" + QuoteLiteral(ip.IPAddress) + ")", nil
	}
	return fmt.Sprintf("(%s, %s)", QuoteLiteral(ip.IPAddress), QuoteLiteral(ip.SubnetMask)), nil
}

// RemoveListener drops the group's listener by DNS name. Run against the
// primary. Existing connections made through the listener are not dropped.
func (ag *AvailabilityGroup) RemoveListener(ctx context.Context, dnsName string) error {
	if strings.TrimSpace(dnsName) == "" {
		return fmt.Errorf("gosmo: remove listener from availability group %q: empty DNS name", ag.Name)
	}
	if err := ag.alter(ctx, "REMOVE LISTENER "+QuoteLiteral(dnsName)); err != nil {
		return fmt.Errorf("gosmo: remove listener %q from availability group %q: %w", dnsName, ag.Name, err)
	}
	return nil
}
