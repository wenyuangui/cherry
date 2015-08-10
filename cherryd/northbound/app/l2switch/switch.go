/*
 * Cherry - An OpenFlow Controller
 *
 * Copyright (C) 2015 Samjung Data Service, Inc. All rights reserved.
 * Kitae Kim <superkkt@sds.co.kr>
 *
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with this program; if not, write to the Free Software Foundation, Inc.,
 * 51 Franklin Street, Fifth Floor, Boston, MA 02110-1301 USA.
 */

package l2switch

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/dlintw/goconf"
	"github.com/superkkt/cherry/cherryd/log"
	"github.com/superkkt/cherry/cherryd/network"
	"github.com/superkkt/cherry/cherryd/northbound/app"
	"github.com/superkkt/cherry/cherryd/openflow"
	"github.com/superkkt/cherry/cherryd/protocol"
	"net"
)

type L2Switch struct {
	app.BaseProcessor
	conf   *goconf.ConfigFile
	log    log.Logger
	vlanID uint16
}

func New(conf *goconf.ConfigFile, log log.Logger) *L2Switch {
	return &L2Switch{
		conf: conf,
		log:  log,
	}
}

func (r *L2Switch) Init() error {
	vlanID, err := r.conf.GetInt("default", "vlan_id")
	if err != nil || vlanID < 0 || vlanID > 4095 {
		return errors.New("invalid default VLAN ID in the config file")
	}
	r.vlanID = uint16(vlanID)

	return nil
}

func (r *L2Switch) Name() string {
	return "L2Switch"
}

func flood(ingress *network.Port, packet []byte) error {
	f := ingress.Device().Factory()

	inPort := openflow.NewInPort()
	inPort.SetValue(ingress.Number())

	outPort := openflow.NewOutPort()
	outPort.SetFlood()

	action, err := f.NewAction()
	if err != nil {
		return err
	}
	action.SetOutPort(outPort)

	out, err := f.NewPacketOut()
	if err != nil {
		return err
	}
	out.SetInPort(inPort)
	out.SetAction(action)
	out.SetData(packet)

	return ingress.Device().SendMessage(out)
}

func isBroadcast(eth *protocol.Ethernet) bool {
	return bytes.Compare(eth.DstMAC, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) == 0
}

type flowParam struct {
	device    *network.Device
	etherType uint16
	inPort    uint32
	outPort   uint32
	srcMAC    net.HardwareAddr
	dstMAC    net.HardwareAddr
}

func (r *flowParam) String() string {
	return fmt.Sprintf("Device=%v, EtherType=%v, InPort=%v, OutPort=%v, SrcMAC=%v, DstMAC=%v", r.device.ID(), r.etherType, r.inPort, r.outPort, r.srcMAC, r.dstMAC)
}

func (r *L2Switch) installFlow(p flowParam) error {
	f := p.device.Factory()

	inPort := openflow.NewInPort()
	inPort.SetValue(p.inPort)
	match, err := f.NewMatch()
	if err != nil {
		return err
	}
	match.SetVLANID(r.vlanID)
	match.SetDstMAC(p.dstMAC)

	outPort := openflow.NewOutPort()
	outPort.SetValue(p.outPort)
	action, err := f.NewAction()
	if err != nil {
		return err
	}
	action.SetOutPort(outPort)
	inst, err := f.NewInstruction()
	if err != nil {
		return err
	}
	inst.ApplyAction(action)

	flow, err := f.NewFlowMod(openflow.FlowAdd)
	if err != nil {
		return err
	}
	flow.SetTableID(p.device.FlowTableID())
	flow.SetIdleTimeout(30)
	flow.SetHardTimeout(600)
	flow.SetPriority(10)
	flow.SetFlowMatch(match)
	flow.SetFlowInstruction(inst)

	return p.device.SendMessage(flow)
}

type switchParam struct {
	finder    network.Finder
	ethernet  *protocol.Ethernet
	ingress   *network.Port
	egress    *network.Port
	rawPacket []byte
}

func (r *L2Switch) switching(p switchParam) error {
	param := flowParam{
		device:    p.ingress.Device(),
		etherType: p.ethernet.Type,
		inPort:    p.ingress.Number(),
		outPort:   p.egress.Number(),
		srcMAC:    p.ethernet.SrcMAC,
		dstMAC:    p.ethernet.DstMAC,
	}
	if err := r.installFlow(param); err != nil {
		return err
	}
	r.log.Debug(fmt.Sprintf("L2Switch: installed a flow rule.. %v", param))

	// Send this ethernet packet directly to the destination node
	r.log.Debug(fmt.Sprintf("L2Switch: sending a packet (Src=%v, Dst=%v) to egress port %v..", p.ethernet.SrcMAC, p.ethernet.DstMAC, p.egress.ID()))
	return r.PacketOut(p.egress, p.rawPacket)
}

func (r *L2Switch) OnPacketIn(finder network.Finder, ingress *network.Port, eth *protocol.Ethernet) error {
	drop, err := r.processPacket(finder, ingress, eth)
	if drop || err != nil {
		return err
	}

	return r.BaseProcessor.OnPacketIn(finder, ingress, eth)
}

func (r *L2Switch) processPacket(finder network.Finder, ingress *network.Port, eth *protocol.Ethernet) (drop bool, err error) {
	r.log.Debug(fmt.Sprintf("L2Switch: PACKET_IN.. Ingress=%v, SrcMAC=%v, DstMAC=%v", ingress.ID(), eth.SrcMAC, eth.DstMAC))

	packet, err := eth.MarshalBinary()
	if err != nil {
		return false, err
	}

	// FIXME: Should we allow broadcasting in here?
	if isBroadcast(eth) {
		r.log.Debug(fmt.Sprintf("L2Switch: broadcasting.. SrcMAC=%v, DstMAC=%v", eth.SrcMAC, eth.DstMAC))
		return true, flood(ingress, packet)
	}

	dstNode, err := finder.Node(eth.DstMAC)
	if err != nil {
		return true, fmt.Errorf("locating a node (MAC=%v): %v", eth.DstMAC, err)
	}
	// Unknown node?
	if dstNode == nil {
		r.log.Debug(fmt.Sprintf("L2Switch: unknown node! dropping.. SrcMAC=%v, DstMAC=%v", eth.SrcMAC, eth.DstMAC))
		return true, nil
	}
	// Disconnected node?
	port := dstNode.Port().Value()
	if port.IsPortDown() || port.IsLinkDown() {
		r.log.Debug(fmt.Sprintf("L2Switch: disconnected node! dropping.. SrcMAC=%v, DstMAC=%v", eth.SrcMAC, eth.DstMAC))
		return true, nil
	}

	param := switchParam{}
	// Check whether src and dst nodes reside on a same switch device
	if ingress.Device().ID() == dstNode.Port().Device().ID() {
		param = switchParam{
			finder:    finder,
			ethernet:  eth,
			ingress:   ingress,
			egress:    dstNode.Port(),
			rawPacket: packet,
		}
	} else {
		path := finder.Path(ingress.Device().ID(), dstNode.Port().Device().ID())
		if len(path) == 0 {
			r.log.Debug(fmt.Sprintf("L2Switch: empty path.. dropping SrcMAC=%v, DstMAC=%v", eth.SrcMAC, eth.DstMAC))
			return true, nil
		}
		egress := path[0][0]
		// Drop this packet if it goes back to the ingress port to avoid duplicated packet routing
		if ingress.Number() == egress.Number() {
			r.log.Debug(fmt.Sprintf("L2Switch: ignore routing path that goes back to the ingress port (SrcMAC=%v, DstMAC=%v)", eth.SrcMAC, eth.DstMAC))
			return true, nil
		}

		param = switchParam{
			finder:    finder,
			ethernet:  eth,
			ingress:   ingress,
			egress:    egress,
			rawPacket: packet,
		}
	}

	return true, r.switching(param)
}

func (r *L2Switch) OnTopologyChange(finder network.Finder) error {
	r.log.Debug("L2Switch: OnTopologyChange..")

	// We should remove all edges from all switch devices when the network topology is changed.
	// Otherwise, installed flow rules in switches may result in incorrect packet routing based on the previous topology.
	if err := r.removeAllFlows(finder.Devices()); err != nil {
		return err
	}

	return r.BaseProcessor.OnTopologyChange(finder)
}

func (r *L2Switch) removeAllFlows(devices []*network.Device) error {
	r.log.Debug("L2Switch: removing all flows from all devices..")

	for _, d := range devices {
		if d.IsClosed() {
			continue
		}

		factory := d.Factory()
		// Wildcard match
		match, err := factory.NewMatch()
		if err != nil {
			return err
		}
		// Set output port to OFPP_NONE
		port := openflow.NewOutPort()
		port.SetNone()

		if err := r.removeFlow(d, match, port); err != nil {
			r.log.Err(fmt.Sprintf("Failed to remove flows on %v: %v", d.ID(), err))
			continue
		}
	}

	return nil
}

func (r *L2Switch) removeFlow(d *network.Device, match openflow.Match, port openflow.OutPort) error {
	r.log.Debug(fmt.Sprintf("L2Switch: removing flows on device %v..", d.ID()))

	f := d.Factory()
	flowmod, err := f.NewFlowMod(openflow.FlowDelete)
	if err != nil {
		return err
	}
	// Remove flows except the table miss flows (Note that MSB of the cookie is a marker)
	flowmod.SetCookieMask(0x1 << 63)
	flowmod.SetTableID(0xFF) // ALL
	flowmod.SetFlowMatch(match)
	flowmod.SetOutPort(port)

	return d.SendMessage(flowmod)
}

func (r *L2Switch) String() string {
	return fmt.Sprintf("%v", r.Name())
}

func (r *L2Switch) OnPortDown(finder network.Finder, port *network.Port) error {
	r.log.Debug(fmt.Sprintf("L2Switch: port down! removing all flows heading to that port (%v)..", port.ID()))

	device := port.Device()
	factory := device.Factory()
	// Wildcard match
	match, err := factory.NewMatch()
	if err != nil {
		return err
	}
	outPort := openflow.NewOutPort()
	outPort.SetValue(port.Number())

	if err := r.removeFlow(device, match, outPort); err != nil {
		return fmt.Errorf("removing flows heading to port %v: %v", port.ID(), err)
	}

	return r.BaseProcessor.OnPortDown(finder, port)
}
