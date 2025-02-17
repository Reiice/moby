package ipam

import (
	"fmt"
	"net"
	"sort"
	"sync"

	"github.com/docker/docker/libnetwork/bitseq"
	"github.com/docker/docker/libnetwork/ipamapi"
	"github.com/docker/docker/libnetwork/types"
	"github.com/sirupsen/logrus"
)

const (
	localAddressSpace  = "LocalDefault"
	globalAddressSpace = "GlobalDefault"
)

// Allocator provides per address space ipv4/ipv6 book keeping
type Allocator struct {
	// Predefined pools for default address spaces
	// Separate from the addrSpace because they should not be serialized
	predefined             map[string][]*net.IPNet
	predefinedStartIndices map[string]int
	// The address spaces
	addrSpaces map[string]*addrSpace
	// Allocated addresses in each address space's subnet
	addresses map[SubnetKey]*bitseq.Handle
	sync.Mutex
}

// NewAllocator returns an instance of libnetwork ipam
func NewAllocator(lcAs, glAs []*net.IPNet) (*Allocator, error) {
	a := &Allocator{
		predefined: map[string][]*net.IPNet{
			localAddressSpace:  lcAs,
			globalAddressSpace: glAs,
		},
		predefinedStartIndices: map[string]int{},
		addresses:              map[SubnetKey]*bitseq.Handle{},
	}

	a.addrSpaces = map[string]*addrSpace{
		localAddressSpace:  a.newAddressSpace(),
		globalAddressSpace: a.newAddressSpace(),
	}

	return a, nil
}

func (a *Allocator) newAddressSpace() *addrSpace {
	return &addrSpace{
		subnets: map[SubnetKey]*PoolData{},
		alloc:   a,
	}
}

// GetDefaultAddressSpaces returns the local and global default address spaces
func (a *Allocator) GetDefaultAddressSpaces() (string, string, error) {
	return localAddressSpace, globalAddressSpace, nil
}

// RequestPool returns an address pool along with its unique id.
// addressSpace must be a valid address space name and must not be the empty string.
// If pool is the empty string then the default predefined pool for addressSpace will be used, otherwise pool must be a valid IP address and length in CIDR notation.
// If subPool is not empty, it must be a valid IP address and length in CIDR notation which is a sub-range of pool.
// subPool must be empty if pool is empty.
func (a *Allocator) RequestPool(addressSpace, pool, subPool string, options map[string]string, v6 bool) (string, *net.IPNet, map[string]string, error) {
	logrus.Debugf("RequestPool(%s, %s, %s, %v, %t)", addressSpace, pool, subPool, options, v6)

	k, nw, ipr, err := a.parsePoolRequest(addressSpace, pool, subPool, v6)
	if err != nil {
		return "", nil, nil, types.InternalErrorf("failed to parse pool request for address space %q pool %q subpool %q: %v", addressSpace, pool, subPool, err)
	}

	pdf := k == nil

retry:
	if pdf {
		if nw, err = a.getPredefinedPool(addressSpace, v6); err != nil {
			return "", nil, nil, err
		}
		k = &SubnetKey{AddressSpace: addressSpace, Subnet: nw.String()}
	}

	aSpace, err := a.getAddrSpace(addressSpace)
	if err != nil {
		return "", nil, nil, err
	}

	insert, err := aSpace.updatePoolDBOnAdd(*k, nw, ipr, pdf)
	if err != nil {
		if _, ok := err.(types.MaskableError); ok {
			logrus.Debugf("Retrying predefined pool search: %v", err)
			goto retry
		}
		return "", nil, nil, err
	}

	return k.String(), nw, nil, insert()
}

// ReleasePool releases the address pool identified by the passed id
func (a *Allocator) ReleasePool(poolID string) error {
	logrus.Debugf("ReleasePool(%s)", poolID)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return err
	}

	return aSpace.updatePoolDBOnRemoval(k)
}

// Given the address space, returns the local or global PoolConfig based on whether the
// address space is local or global. AddressSpace locality is registered with IPAM out of band.
func (a *Allocator) getAddrSpace(as string) (*addrSpace, error) {
	a.Lock()
	defer a.Unlock()
	aSpace, ok := a.addrSpaces[as]
	if !ok {
		return nil, types.BadRequestErrorf("cannot find address space %s", as)
	}
	return aSpace, nil
}

// parsePoolRequest parses and validates a request to create a new pool under addressSpace and returns
// a SubnetKey, network and range describing the request.
func (a *Allocator) parsePoolRequest(addressSpace, pool, subPool string, v6 bool) (*SubnetKey, *net.IPNet, *AddressRange, error) {
	var (
		nw  *net.IPNet
		ipr *AddressRange
		err error
	)

	if addressSpace == "" {
		return nil, nil, nil, ipamapi.ErrInvalidAddressSpace
	}

	if pool == "" && subPool != "" {
		return nil, nil, nil, ipamapi.ErrInvalidSubPool
	}

	if pool == "" {
		return nil, nil, nil, nil
	}

	if _, nw, err = net.ParseCIDR(pool); err != nil {
		return nil, nil, nil, ipamapi.ErrInvalidPool
	}

	if subPool != "" {
		if ipr, err = getAddressRange(subPool, nw); err != nil {
			return nil, nil, nil, err
		}
	}

	return &SubnetKey{AddressSpace: addressSpace, Subnet: nw.String(), ChildSubnet: subPool}, nw, ipr, nil
}

func (a *Allocator) insertBitMask(key SubnetKey, pool *net.IPNet) error {
	//logrus.Debugf("Inserting bitmask (%s, %s)", key.String(), pool.String())

	ipVer := getAddressVersion(pool.IP)
	ones, bits := pool.Mask.Size()
	numAddresses := uint64(1 << uint(bits-ones))

	// Allow /64 subnet
	if ipVer == v6 && numAddresses == 0 {
		numAddresses--
	}

	// Generate the new address masks.
	h, err := bitseq.NewHandle("", nil, "", numAddresses)
	if err != nil {
		return err
	}

	// Pre-reserve the network address on IPv4 networks large
	// enough to have one (i.e., anything bigger than a /31.
	if !(ipVer == v4 && numAddresses <= 2) {
		h.Set(0)
	}

	// Pre-reserve the broadcast address on IPv4 networks large
	// enough to have one (i.e., anything bigger than a /31).
	if ipVer == v4 && numAddresses > 2 {
		h.Set(numAddresses - 1)
	}

	a.Lock()
	a.addresses[key] = h
	a.Unlock()
	return nil
}

func (a *Allocator) retrieveBitmask(k SubnetKey, n *net.IPNet) (*bitseq.Handle, error) {
	a.Lock()
	bm, ok := a.addresses[k]
	a.Unlock()
	if !ok {
		logrus.Debugf("Retrieving bitmask (%s, %s)", k, n)
		if err := a.insertBitMask(k, n); err != nil {
			return nil, types.InternalErrorf("could not find bitmask for %s", k.String())
		}
		a.Lock()
		bm = a.addresses[k]
		a.Unlock()
	}
	return bm, nil
}

func (a *Allocator) getPredefineds(as string) []*net.IPNet {
	a.Lock()
	defer a.Unlock()

	p := a.predefined[as]
	i := a.predefinedStartIndices[as]
	// defensive in case the list changed since last update
	if i >= len(p) {
		i = 0
	}
	return append(p[i:], p[:i]...)
}

func (a *Allocator) updateStartIndex(as string, amt int) {
	a.Lock()
	i := a.predefinedStartIndices[as] + amt
	if i < 0 || i >= len(a.predefined[as]) {
		i = 0
	}
	a.predefinedStartIndices[as] = i
	a.Unlock()
}

func (a *Allocator) getPredefinedPool(as string, ipV6 bool) (*net.IPNet, error) {
	var v ipVersion
	v = v4
	if ipV6 {
		v = v6
	}

	if as != localAddressSpace && as != globalAddressSpace {
		return nil, types.NotImplementedErrorf("no default pool available for non-default address spaces")
	}

	aSpace, err := a.getAddrSpace(as)
	if err != nil {
		return nil, err
	}

	predefined := a.getPredefineds(as)

	aSpace.Lock()
	for i, nw := range predefined {
		if v != getAddressVersion(nw.IP) {
			continue
		}
		// Checks whether pool has already been allocated
		if _, ok := aSpace.subnets[SubnetKey{AddressSpace: as, Subnet: nw.String()}]; ok {
			continue
		}
		// Shouldn't be necessary, but check prevents IP collisions should
		// predefined pools overlap for any reason.
		if !aSpace.contains(as, nw) {
			aSpace.Unlock()
			a.updateStartIndex(as, i+1)
			return nw, nil
		}
	}
	aSpace.Unlock()

	return nil, types.NotFoundErrorf("could not find an available, non-overlapping IPv%d address pool among the defaults to assign to the network", v)
}

// RequestAddress returns an address from the specified pool ID
func (a *Allocator) RequestAddress(poolID string, prefAddress net.IP, opts map[string]string) (*net.IPNet, map[string]string, error) {
	logrus.Debugf("RequestAddress(%s, %v, %v)", poolID, prefAddress, opts)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return nil, nil, types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return nil, nil, err
	}

	aSpace.Lock()
	p, ok := aSpace.subnets[k]
	if !ok {
		aSpace.Unlock()
		return nil, nil, types.NotFoundErrorf("cannot find address pool for poolID:%s", poolID)
	}

	if prefAddress != nil && !p.Pool.Contains(prefAddress) {
		aSpace.Unlock()
		return nil, nil, ipamapi.ErrIPOutOfRange
	}

	c := p
	for c.Range != nil {
		k = c.ParentKey
		c = aSpace.subnets[k]
	}
	aSpace.Unlock()

	bm, err := a.retrieveBitmask(k, c.Pool)
	if err != nil {
		return nil, nil, types.InternalErrorf("could not find bitmask for %s on address %v request from pool %s: %v",
			k.String(), prefAddress, poolID, err)
	}
	// In order to request for a serial ip address allocation, callers can pass in the option to request
	// IP allocation serially or first available IP in the subnet
	var serial bool
	if opts != nil {
		if val, ok := opts[ipamapi.AllocSerialPrefix]; ok {
			serial = (val == "true")
		}
	}
	ip, err := a.getAddress(p.Pool, bm, prefAddress, p.Range, serial)
	if err != nil {
		return nil, nil, err
	}

	return &net.IPNet{IP: ip, Mask: p.Pool.Mask}, nil, nil
}

// ReleaseAddress releases the address from the specified pool ID
func (a *Allocator) ReleaseAddress(poolID string, address net.IP) error {
	logrus.Debugf("ReleaseAddress(%s, %v)", poolID, address)
	k := SubnetKey{}
	if err := k.FromString(poolID); err != nil {
		return types.BadRequestErrorf("invalid pool id: %s", poolID)
	}

	aSpace, err := a.getAddrSpace(k.AddressSpace)
	if err != nil {
		return err
	}

	aSpace.Lock()
	p, ok := aSpace.subnets[k]
	if !ok {
		aSpace.Unlock()
		return types.NotFoundErrorf("cannot find address pool for poolID:%s", poolID)
	}

	if address == nil {
		aSpace.Unlock()
		return types.BadRequestErrorf("invalid address: nil")
	}

	if !p.Pool.Contains(address) {
		aSpace.Unlock()
		return ipamapi.ErrIPOutOfRange
	}

	c := p
	for c.Range != nil {
		k = c.ParentKey
		c = aSpace.subnets[k]
	}
	aSpace.Unlock()

	mask := p.Pool.Mask

	h, err := types.GetHostPartIP(address, mask)
	if err != nil {
		return types.InternalErrorf("failed to release address %s: %v", address.String(), err)
	}

	bm, err := a.retrieveBitmask(k, c.Pool)
	if err != nil {
		return types.InternalErrorf("could not find bitmask for %s on address %v release from pool %s: %v",
			k.String(), address, poolID, err)
	}
	defer logrus.Debugf("Released address PoolID:%s, Address:%v Sequence:%s", poolID, address, bm)

	return bm.Unset(ipToUint64(h))
}

func (a *Allocator) getAddress(nw *net.IPNet, bitmask *bitseq.Handle, prefAddress net.IP, ipr *AddressRange, serial bool) (net.IP, error) {
	var (
		ordinal uint64
		err     error
		base    *net.IPNet
	)

	logrus.Debugf("Request address PoolID:%v %s Serial:%v PrefAddress:%v ", nw, bitmask, serial, prefAddress)
	base = types.GetIPNetCopy(nw)

	if bitmask.Unselected() == 0 {
		return nil, ipamapi.ErrNoAvailableIPs
	}
	if ipr == nil && prefAddress == nil {
		ordinal, err = bitmask.SetAny(serial)
	} else if prefAddress != nil {
		hostPart, e := types.GetHostPartIP(prefAddress, base.Mask)
		if e != nil {
			return nil, types.InternalErrorf("failed to allocate requested address %s: %v", prefAddress.String(), e)
		}
		ordinal = ipToUint64(types.GetMinimalIP(hostPart))
		err = bitmask.Set(ordinal)
	} else {
		ordinal, err = bitmask.SetAnyInRange(ipr.Start, ipr.End, serial)
	}

	switch err {
	case nil:
		// Convert IP ordinal for this subnet into IP address
		return generateAddress(ordinal, base), nil
	case bitseq.ErrBitAllocated:
		return nil, ipamapi.ErrIPAlreadyAllocated
	case bitseq.ErrNoBitAvailable:
		return nil, ipamapi.ErrNoAvailableIPs
	default:
		return nil, err
	}
}

// DumpDatabase dumps the internal info
func (a *Allocator) DumpDatabase() string {
	a.Lock()
	aspaces := make(map[string]*addrSpace, len(a.addrSpaces))
	orderedAS := make([]string, 0, len(a.addrSpaces))
	for as, aSpace := range a.addrSpaces {
		orderedAS = append(orderedAS, as)
		aspaces[as] = aSpace
	}
	a.Unlock()

	sort.Strings(orderedAS)

	var s string
	for _, as := range orderedAS {
		aSpace := aspaces[as]
		s = fmt.Sprintf("\n\n%s Config", as)
		aSpace.Lock()
		for k, config := range aSpace.subnets {
			s += fmt.Sprintf("\n%v: %v", k, config)
			if config.Range == nil {
				a.retrieveBitmask(k, config.Pool)
			}
		}
		aSpace.Unlock()
	}

	s = fmt.Sprintf("%s\n\nBitmasks", s)
	for k, bm := range a.addresses {
		s += fmt.Sprintf("\n%s: %s", k, bm)
	}

	return s
}

// IsBuiltIn returns true for builtin drivers
func (a *Allocator) IsBuiltIn() bool {
	return true
}
