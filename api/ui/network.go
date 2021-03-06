/*
 * Cherry - An OpenFlow Controller
 *
 * Copyright (C) 2015-2019 Samjung Data Service, Inc. All rights reserved.
 *
 *  Kitae Kim <superkkt@sds.co.kr>
 *  Donam Kim <donam.kim@sds.co.kr>
 *  Jooyoung Kang <jooyoung.kang@sds.co.kr>
 *  Changjin Choi <ccj9707@sds.co.kr>
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

package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/superkkt/cherry/api"
	"github.com/superkkt/cherry/network"

	"github.com/ant0ine/go-json-rest/rest"
	"github.com/davecgh/go-spew/spew"
)

type NetworkTransaction interface {
	// Networks returns a list of registered networks. Address can be nil that means no address search. Pagination limit can be 0 that means no pagination.
	Networks(address *string, pagination Pagination) ([]*Network, error)
	AddNetwork(requesterID uint64, addr net.IP, mask net.IPMask, gateway net.IP) (network *Network, duplicated bool, err error)
	// RemoveNetwork removes a network specified by id and then returns information of the network before removing. It returns nil if the network does not exist.
	RemoveNetwork(requesterID, netID uint64) (*Network, error)
}

type Network struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"` // FIXME: Use a native type.
	Mask    uint8  `json:"mask"`    // FIXME: Use a native type.
	Gateway string `json:"gateway"` // FIXME: Use a native type.
}

func (r *API) listNetwork(w api.ResponseWriter, req *rest.Request) {
	p := new(listNetworkParam)
	if err := req.DecodeJsonPayload(p); err != nil {
		w.Write(api.Response{Status: api.StatusInvalidParameter, Message: fmt.Sprintf("failed to decode param: %v", err.Error())})
		return
	}
	logger.Debugf("listNetwork request from %v: %v", req.RemoteAddr, spew.Sdump(p))

	if _, ok := r.session.Get(p.SessionID); ok == false {
		w.Write(api.Response{Status: api.StatusUnknownSession, Message: fmt.Sprintf("unknown session id: %v", p.SessionID)})
		return
	}

	var network []*Network
	f := func(tx Transaction) (err error) {
		network, err = tx.Networks(p.Address, p.Pagination)
		return err
	}
	if err := r.DB.Exec(f); err != nil {
		w.Write(api.Response{Status: api.StatusInternalServerError, Message: fmt.Sprintf("failed to query the network list: %v", err.Error())})
		return
	}
	logger.Debugf("queried network list: %v", spew.Sdump(network))

	w.Write(api.Response{Status: api.StatusOkay, Data: network})
}

type listNetworkParam struct {
	SessionID  string
	Address    *string
	Pagination Pagination
}

func (r *listNetworkParam) UnmarshalJSON(data []byte) error {
	v := struct {
		SessionID  string     `json:"session_id"`
		Address    *string    `json:"address"`
		Pagination Pagination `json:"pagination"`
	}{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*r = listNetworkParam(v)

	return r.validate()
}

func (r *listNetworkParam) validate() error {
	if len(r.SessionID) != 64 {
		return errors.New("invalid session id")
	}
	if r.Address != nil {
		if err := validateIP(*r.Address); err != nil {
			return err
		}
	}

	return nil
}

func (r *API) addNetwork(w api.ResponseWriter, req *rest.Request) {
	p := new(addNetworkParam)
	if err := req.DecodeJsonPayload(p); err != nil {
		w.Write(api.Response{Status: api.StatusInvalidParameter, Message: fmt.Sprintf("failed to decode param: %v", err.Error())})
		return
	}
	logger.Debugf("addNetwork request from %v: %v", req.RemoteAddr, spew.Sdump(p))

	session, ok := r.session.Get(p.SessionID)
	if ok == false {
		w.Write(api.Response{Status: api.StatusUnknownSession, Message: fmt.Sprintf("unknown session id: %v", p.SessionID)})
		return
	}

	var network *Network
	var duplicated bool
	f := func(tx Transaction) (err error) {
		network, duplicated, err = tx.AddNetwork(session.(*User).ID, p.Address, p.Mask, p.Gateway)
		return err
	}
	if err := r.DB.Exec(f); err != nil {
		w.Write(api.Response{Status: api.StatusInternalServerError, Message: fmt.Sprintf("failed to add a new network: %v", err.Error())})
		return
	}

	if duplicated {
		w.Write(api.Response{Status: api.StatusDuplicated, Message: fmt.Sprintf("duplicated network: address=%v, mask=%v", p.Address, p.Mask)})
		return
	}
	logger.Debugf("added network info: %v", spew.Sdump(network))

	w.Write(api.Response{Status: api.StatusOkay, Data: network})
}

type addNetworkParam struct {
	SessionID string
	Address   net.IP
	Mask      net.IPMask
	Gateway   net.IP
}

func (r *addNetworkParam) UnmarshalJSON(data []byte) error {
	v := struct {
		SessionID string `json:"session_id"`
		Address   string `json:"address"`
		Mask      uint8  `json:"mask"`
		Gateway   string `json:"gateway"`
	}{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	if len(v.SessionID) != 64 {
		return errors.New("invalid session id")
	}
	if v.Mask < 24 || v.Mask > 30 {
		return fmt.Errorf("invalid network mask: %v", v.Mask)
	}
	_, network, err := net.ParseCIDR(fmt.Sprintf("%v/%v", v.Address, v.Mask))
	if err != nil || network == nil {
		return fmt.Errorf("invalid network address: %v", v.Address)
	}
	gateway := net.ParseIP(v.Gateway)
	if gateway == nil {
		return fmt.Errorf("invalid network gateway: %v", v.Gateway)
	}
	if err := validateGateway(*network, gateway); err != nil {
		return err
	}

	r.SessionID = v.SessionID
	r.Mask = network.Mask
	r.Address = network.IP
	r.Gateway = gateway

	return nil
}

func validateGateway(n net.IPNet, g net.IP) error {
	invalid := fmt.Errorf("invalid network gateway: %v", g)
	broadcast := net.IP(make([]byte, 4))
	for i := range n.IP.To4() {
		broadcast[i] = n.IP.To4()[i] | ^n.Mask[i]
	}

	reserved, err := network.ReservedIP(n)
	if err != nil {
		return err
	}

	if n.Contains(g) == false {
		return invalid
	}
	if g.Equal(n.IP) || g.Equal(broadcast) || g.Equal(reserved) {
		return invalid
	}

	return nil
}

func (r *API) removeNetwork(w api.ResponseWriter, req *rest.Request) {
	p := new(removeNetworkParam)
	if err := req.DecodeJsonPayload(p); err != nil {
		w.Write(api.Response{Status: api.StatusInvalidParameter, Message: fmt.Sprintf("failed to decode param: %v", err.Error())})
		return
	}
	logger.Debugf("removeNetwork request from %v: %v", req.RemoteAddr, spew.Sdump(p))

	session, ok := r.session.Get(p.SessionID)
	if ok == false {
		w.Write(api.Response{Status: api.StatusUnknownSession, Message: fmt.Sprintf("unknown session id: %v", p.SessionID)})
		return
	}

	var network *Network
	f := func(tx Transaction) (err error) {
		network, err = tx.RemoveNetwork(session.(*User).ID, p.ID)
		return err
	}
	if err := r.DB.Exec(f); err != nil {
		w.Write(api.Response{Status: api.StatusInternalServerError, Message: fmt.Sprintf("failed to remove a network: %v", err.Error())})
		return
	}

	if network == nil {
		w.Write(api.Response{Status: api.StatusNotFound, Message: fmt.Sprintf("not found network to remove: %v", p.ID)})
		return
	}
	logger.Debugf("removed a network: %v", spew.Sdump(network))

	logger.Debug("removing all flows from the entire switches")
	if err := r.Controller.RemoveFlows(); err != nil {
		// Ignore this error.
		logger.Errorf("failed to remove flows: %v", err)
	} else {
		logger.Debug("removed all flows from the entire switches")
	}

	w.Write(api.Response{Status: api.StatusOkay})
}

type removeNetworkParam struct {
	SessionID string
	ID        uint64
}

func (r *removeNetworkParam) UnmarshalJSON(data []byte) error {
	v := struct {
		SessionID string `json:"session_id"`
		ID        uint64 `json:"id"`
	}{}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*r = removeNetworkParam(v)

	return r.validate()
}

func (r *removeNetworkParam) validate() error {
	if len(r.SessionID) != 64 {
		return errors.New("invalid session id")
	}
	if r.ID == 0 {
		return errors.New("empty network id")
	}

	return nil
}
