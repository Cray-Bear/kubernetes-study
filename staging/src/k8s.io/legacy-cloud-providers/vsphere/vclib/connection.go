/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vclib

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	neturl "net/url"
	"sync"

	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/sts"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
	"k8s.io/klog/v2"

	"k8s.io/client-go/pkg/version"
)

// VSphereConnection contains information for connecting to vCenter
type VSphereConnection struct {
	Client            *vim25.Client
	Username          string
	Password          string `datapolicy:"password"`
	Hostname          string
	Port              string
	CACert            string
	Thumbprint        string
	Insecure          bool
	RoundTripperCount uint
	credentialsLock   sync.Mutex
}

var (
	clientLock sync.Mutex
)

// Connect makes connection to vCenter and sets VSphereConnection.Client.
// If connection.Client is already set, it obtains the existing user session.
// if user session is not valid, connection.Client will be set to the new client.
func (connection *VSphereConnection) Connect(ctx context.Context) error {
	var err error
	clientLock.Lock()
	defer clientLock.Unlock()

	if connection.Client == nil {
		connection.Client, err = connection.NewClient(ctx)
		if err != nil {
			klog.Errorf("Failed to create govmomi client. err: %+v", err)
			return err
		}
		setVCenterInfoMetric(connection)
		return nil
	}
	m := session.NewManager(connection.Client)
	userSession, err := m.UserSession(ctx)
	if err != nil {
		klog.Errorf("Error while obtaining user session. err: %+v", err)
		return err
	}
	if userSession != nil {
		return nil
	}
	klog.Warningf("Creating new client session since the existing session is not valid or not authenticated")

	connection.Client, err = connection.NewClient(ctx)
	if err != nil {
		klog.Errorf("Failed to create govmomi client. err: %+v", err)
		return err
	}
	return nil
}

// Signer returns an sts.Signer for use with SAML token auth if connection is configured for such.
// Returns nil if username/password auth is configured for the connection.
func (connection *VSphereConnection) Signer(ctx context.Context, client *vim25.Client) (*sts.Signer, error) {
	// TODO: Add separate fields for certificate and private-key.
	// For now we can leave the config structs and validation as-is and
	// decide to use LoginByToken if the username value is PEM encoded.
	b, _ := pem.Decode([]byte(connection.Username))
	if b == nil {
		return nil, nil
	}

	cert, err := tls.X509KeyPair([]byte(connection.Username), []byte(connection.Password))
	if err != nil {
		klog.Errorf("Failed to load X509 key pair. err: %+v", err)
		return nil, err
	}

	tokens, err := sts.NewClient(ctx, client)
	if err != nil {
		klog.Errorf("Failed to create STS client. err: %+v", err)
		return nil, err
	}

	req := sts.TokenRequest{
		Certificate: &cert,
		Delegatable: true,
	}

	signer, err := tokens.Issue(ctx, req)
	if err != nil {
		klog.Errorf("Failed to issue SAML token. err: %+v", err)
		return nil, err
	}

	return signer, nil
}

// login calls SessionManager.LoginByToken if certificate and private key are configured,
// otherwise calls SessionManager.Login with user and password.
func (connection *VSphereConnection) login(ctx context.Context, client *vim25.Client) error {
	m := session.NewManager(client)
	connection.credentialsLock.Lock()
	defer connection.credentialsLock.Unlock()

	signer, err := connection.Signer(ctx, client)
	if err != nil {
		return err
	}

	if signer == nil {
		klog.V(3).Infof("SessionManager.Login with username %q", connection.Username)
		return m.Login(ctx, neturl.UserPassword(connection.Username, connection.Password))
	}

	klog.V(3).Infof("SessionManager.LoginByToken with certificate %q", connection.Username)

	header := soap.Header{Security: signer}

	return m.LoginByToken(client.WithHeader(ctx, header))
}

// Logout calls SessionManager.Logout for the given connection.
func (connection *VSphereConnection) Logout(ctx context.Context) {
	clientLock.Lock()
	c := connection.Client
	clientLock.Unlock()
	if c == nil {
		return
	}

	m := session.NewManager(c)

	hasActiveSession, err := m.SessionIsActive(ctx)
	if err != nil {
		klog.Errorf("Logout failed: %s", err)
		return
	}
	if !hasActiveSession {
		klog.Errorf("No active session, cannot logout")
		return
	}
	if err := m.Logout(ctx); err != nil {
		klog.Errorf("Logout failed: %s", err)
	}
}

// NewClient creates a new govmomi client for the VSphereConnection obj
func (connection *VSphereConnection) NewClient(ctx context.Context) (*vim25.Client, error) {
	url, err := soap.ParseURL(net.JoinHostPort(connection.Hostname, connection.Port))
	if err != nil {
		klog.Errorf("Failed to parse URL: %s. err: %+v", url, err)
		return nil, err
	}

	sc := soap.NewClient(url, connection.Insecure)

	if ca := connection.CACert; ca != "" {
		if err := sc.SetRootCAs(ca); err != nil {
			return nil, err
		}
	}

	tpHost := connection.Hostname + ":" + connection.Port
	sc.SetThumbprint(tpHost, connection.Thumbprint)

	client, err := vim25.NewClient(ctx, sc)
	if err != nil {
		klog.Errorf("Failed to create new client. err: %+v", err)
		return nil, err
	}

	k8sVersion := version.Get().GitVersion
	client.UserAgent = fmt.Sprintf("kubernetes-cloudprovider/%s", k8sVersion)

	err = connection.login(ctx, client)
	if err != nil {
		return nil, err
	}
	klogV := klog.V(3)
	if klogV.Enabled() {
		s, err := session.NewManager(client).UserSession(ctx)
		if err == nil {
			klogV.Infof("New session ID for '%s' = %s", s.UserName, s.Key)
		}
	}

	if connection.RoundTripperCount == 0 {
		connection.RoundTripperCount = RoundTripperDefaultCount
	}
	client.RoundTripper = vim25.Retry(client.RoundTripper, vim25.TemporaryNetworkError(int(connection.RoundTripperCount)))
	vcdeprecated, err := isvCenterDeprecated(client.ServiceContent.About.Version, client.ServiceContent.About.ApiVersion)
	if err != nil {
		klog.Errorf("failed to check if vCenter version:%v and api version: %s is deprecated. Error: %v", client.ServiceContent.About.Version, client.ServiceContent.About.ApiVersion, err)
	}
	if vcdeprecated {
		// After this deprecation, vSphere 6.5 support period is extended to October 15, 2022 as
		// https://blogs.vmware.com/vsphere/2021/03/announcing-limited-extension-of-vmware-vsphere-6-5-general-support-period.html
		// In addition, the external vSphere cloud provider does not support vSphere 6.5.
		// Please keep vSphere 6.5 support til the period.
		klog.Warningf("vCenter is deprecated. version: %s, api verson: %s Please consider upgrading vCenter and ESXi servers to 6.7u3 or higher", client.ServiceContent.About.Version, client.ServiceContent.About.ApiVersion)
	}
	return client, nil
}

// UpdateCredentials updates username and password.
// Note: Updated username and password will be used when there is no session active
func (connection *VSphereConnection) UpdateCredentials(username string, password string) {
	connection.credentialsLock.Lock()
	defer connection.credentialsLock.Unlock()
	connection.Username = username
	connection.Password = password
}
