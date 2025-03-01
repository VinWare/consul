package consul

import (
	"context"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	uuid "github.com/hashicorp/go-uuid"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/connect/ca"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/agent/token"
	"github.com/hashicorp/consul/sdk/testutil/retry"
	"github.com/hashicorp/consul/testrpc"
)

func TestLeader_Builtin_PrimaryCA_ChangeKeyConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	types := []struct {
		keyType string
		keyBits int
	}{
		{connect.DefaultPrivateKeyType, connect.DefaultPrivateKeyBits},
		{"ec", 256},
		{"ec", 384},
		{"rsa", 2048},
		{"rsa", 4096},
	}

	for _, src := range types {
		for _, dst := range types {
			if src == dst {
				continue // skip
			}
			src := src
			dst := dst
			t.Run(fmt.Sprintf("%s-%d to %s-%d", src.keyType, src.keyBits, dst.keyType, dst.keyBits), func(t *testing.T) {
				t.Parallel()

				providerState := map[string]string{"foo": "dc1-value"}

				// Initialize primary as the primary DC
				dir1, srv := testServerWithConfig(t, func(c *Config) {
					c.Datacenter = "dc1"
					c.PrimaryDatacenter = "dc1"
					c.Build = "1.6.0"
					c.CAConfig.Config["PrivateKeyType"] = src.keyType
					c.CAConfig.Config["PrivateKeyBits"] = src.keyBits
					c.CAConfig.Config["test_state"] = providerState
				})
				defer os.RemoveAll(dir1)
				defer srv.Shutdown()
				codec := rpcClient(t, srv)
				defer codec.Close()

				testrpc.WaitForLeader(t, srv.RPC, "dc1")
				testrpc.WaitForActiveCARoot(t, srv.RPC, "dc1", nil)

				var (
					provider ca.Provider
					caRoot   *structs.CARoot
				)
				retry.Run(t, func(r *retry.R) {
					provider, caRoot = getCAProviderWithLock(srv)
					require.NotNil(r, caRoot)
					// Sanity check CA is using the correct key type
					require.Equal(r, src.keyType, caRoot.PrivateKeyType)
					require.Equal(r, src.keyBits, caRoot.PrivateKeyBits)
				})

				runStep(t, "sign leaf cert and make sure chain is correct", func(t *testing.T) {
					spiffeService := &connect.SpiffeIDService{
						Host:       "node1",
						Namespace:  "default",
						Datacenter: "dc1",
						Service:    "foo",
					}
					raw, _ := connect.TestCSR(t, spiffeService)

					leafCsr, err := connect.ParseCSR(raw)
					require.NoError(t, err)

					leafPEM, err := provider.Sign(leafCsr)
					require.NoError(t, err)

					// Check that the leaf signed by the new cert can be verified using the
					// returned cert chain
					require.NoError(t, connect.ValidateLeaf(caRoot.RootCert, leafPEM, []string{}))
				})

				runStep(t, "verify persisted state is correct", func(t *testing.T) {
					state := srv.fsm.State()
					_, caConfig, err := state.CAConfig(nil)
					require.NoError(t, err)
					require.Equal(t, providerState, caConfig.State)
				})

				runStep(t, "change roots", func(t *testing.T) {
					// Update a config value
					newConfig := &structs.CAConfiguration{
						Provider: "consul",
						Config: map[string]interface{}{
							"PrivateKey":     "",
							"RootCert":       "",
							"PrivateKeyType": dst.keyType,
							"PrivateKeyBits": dst.keyBits,
							// This verifies the state persistence for providers although Consul
							// provider doesn't actually use that mechanism outside of tests.
							"test_state": providerState,
						},
					}

					args := &structs.CARequest{
						Datacenter: "dc1",
						Config:     newConfig,
					}
					var reply interface{}
					require.NoError(t, msgpackrpc.CallWithCodec(codec, "ConnectCA.ConfigurationSet", args, &reply))
				})

				var (
					newProvider ca.Provider
					newCaRoot   *structs.CARoot
				)
				retry.Run(t, func(r *retry.R) {
					newProvider, newCaRoot = getCAProviderWithLock(srv)
					require.NotNil(r, newCaRoot)
					// Sanity check CA is using the correct key type
					require.Equal(r, dst.keyType, newCaRoot.PrivateKeyType)
					require.Equal(r, dst.keyBits, newCaRoot.PrivateKeyBits)
				})

				runStep(t, "sign leaf cert and make sure NEW chain is correct", func(t *testing.T) {
					spiffeService := &connect.SpiffeIDService{
						Host:       "node1",
						Namespace:  "default",
						Datacenter: "dc1",
						Service:    "foo",
					}
					raw, _ := connect.TestCSR(t, spiffeService)

					leafCsr, err := connect.ParseCSR(raw)
					require.NoError(t, err)

					leafPEM, err := newProvider.Sign(leafCsr)
					require.NoError(t, err)

					// Check that the leaf signed by the new cert can be verified using the
					// returned cert chain
					require.NoError(t, connect.ValidateLeaf(newCaRoot.RootCert, leafPEM, []string{}))
				})

				runStep(t, "verify persisted state is still correct", func(t *testing.T) {
					state := srv.fsm.State()
					_, caConfig, err := state.CAConfig(nil)
					require.NoError(t, err)
					require.Equal(t, providerState, caConfig.State)
				})
			})
		}
	}

}

func TestLeader_SecondaryCA_Initialize(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	tests := []struct {
		keyType string
		keyBits int
	}{
		{connect.DefaultPrivateKeyType, connect.DefaultPrivateKeyBits},
		{"rsa", 2048},
	}

	dc1State := map[string]string{"foo": "dc1-value"}
	dc2State := map[string]string{"foo": "dc2-value"}

	for _, tc := range tests {
		tc := tc
		t.Run(fmt.Sprintf("%s-%d", tc.keyType, tc.keyBits), func(t *testing.T) {
			masterToken := "8a85f086-dd95-4178-b128-e10902767c5c"

			// Initialize primary as the primary DC
			dir1, s1 := testServerWithConfig(t, func(c *Config) {
				c.Datacenter = "primary"
				c.PrimaryDatacenter = "primary"
				c.Build = "1.6.0"
				c.ACLsEnabled = true
				c.ACLMasterToken = masterToken
				c.ACLResolverSettings.ACLDefaultPolicy = "deny"
				c.CAConfig.Config["PrivateKeyType"] = tc.keyType
				c.CAConfig.Config["PrivateKeyBits"] = tc.keyBits
				c.CAConfig.Config["test_state"] = dc1State
			})
			defer os.RemoveAll(dir1)
			defer s1.Shutdown()

			s1.tokens.UpdateAgentToken(masterToken, token.TokenSourceConfig)

			testrpc.WaitForLeader(t, s1.RPC, "primary")

			// secondary as a secondary DC
			dir2, s2 := testServerWithConfig(t, func(c *Config) {
				c.Datacenter = "secondary"
				c.PrimaryDatacenter = "primary"
				c.Build = "1.6.0"
				c.ACLsEnabled = true
				c.ACLResolverSettings.ACLDefaultPolicy = "deny"
				c.ACLTokenReplication = true
				c.CAConfig.Config["PrivateKeyType"] = tc.keyType
				c.CAConfig.Config["PrivateKeyBits"] = tc.keyBits
				c.CAConfig.Config["test_state"] = dc2State
			})
			defer os.RemoveAll(dir2)
			defer s2.Shutdown()

			s2.tokens.UpdateAgentToken(masterToken, token.TokenSourceConfig)
			s2.tokens.UpdateReplicationToken(masterToken, token.TokenSourceConfig)

			// Create the WAN link
			joinWAN(t, s2, s1)

			testrpc.WaitForLeader(t, s2.RPC, "secondary")

			// Ensure s2 is authoritative.
			waitForNewACLReplication(t, s2, structs.ACLReplicateTokens, 1, 1, 0)

			// Wait until the providers are fully bootstrapped.
			var (
				caRoot            *structs.CARoot
				secondaryProvider ca.Provider
				intermediatePEM   string
				err               error
			)
			retry.Run(t, func(r *retry.R) {
				_, caRoot = getCAProviderWithLock(s1)
				secondaryProvider, _ = getCAProviderWithLock(s2)
				intermediatePEM, err = secondaryProvider.ActiveIntermediate()
				require.NoError(r, err)

				// Sanity check CA is using the correct key type
				require.Equal(r, tc.keyType, caRoot.PrivateKeyType)
				require.Equal(r, tc.keyBits, caRoot.PrivateKeyBits)

				// Verify the root lists are equal in each DC's state store.
				state1 := s1.fsm.State()
				_, roots1, err := state1.CARoots(nil)
				require.NoError(r, err)

				state2 := s2.fsm.State()
				_, roots2, err := state2.CARoots(nil)
				require.NoError(r, err)
				require.Len(r, roots1, 1)
				require.Len(r, roots2, 1)
				require.Equal(r, roots1[0].ID, roots2[0].ID)
				require.Equal(r, roots1[0].RootCert, roots2[0].RootCert)
				require.Empty(r, roots1[0].IntermediateCerts)
				require.NotEmpty(r, roots2[0].IntermediateCerts)
			})

			// Have secondary sign a leaf cert and make sure the chain is correct.
			spiffeService := &connect.SpiffeIDService{
				Host:       "node1",
				Namespace:  "default",
				Datacenter: "primary",
				Service:    "foo",
			}
			raw, _ := connect.TestCSR(t, spiffeService)

			leafCsr, err := connect.ParseCSR(raw)
			require.NoError(t, err)

			leafPEM, err := secondaryProvider.Sign(leafCsr)
			require.NoError(t, err)

			// Check that the leaf signed by the new cert can be verified using the
			// returned cert chain (signed intermediate + remote root).
			require.NoError(t, connect.ValidateLeaf(caRoot.RootCert, leafPEM, []string{intermediatePEM}))

			// Verify that both primary and secondary persisted state as expected -
			// pass through from the config.
			{
				state := s1.fsm.State()
				_, caConfig, err := state.CAConfig(nil)
				require.NoError(t, err)
				require.Equal(t, dc1State, caConfig.State)
			}
			{
				state := s2.fsm.State()
				_, caConfig, err := state.CAConfig(nil)
				require.NoError(t, err)
				require.Equal(t, dc2State, caConfig.State)
			}

		})
	}
}

func waitForActiveCARoot(t *testing.T, srv *Server, expect *structs.CARoot) {
	retry.Run(t, func(r *retry.R) {
		_, root := getCAProviderWithLock(srv)
		if root == nil {
			r.Fatal("no root")
		}
		if root.ID != expect.ID {
			r.Fatalf("current active root is %s; waiting for %s", root.ID, expect.ID)
		}
	})
}

func getCAProviderWithLock(s *Server) (ca.Provider, *structs.CARoot) {
	return s.caManager.getCAProvider()
}

func TestLeader_Vault_PrimaryCA_IntermediateRenew(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	ca.SkipIfVaultNotPresent(t)

	// no parallel execution because we change globals
	origInterval := structs.IntermediateCertRenewInterval
	origMinTTL := structs.MinLeafCertTTL
	origDriftBuffer := ca.CertificateTimeDriftBuffer
	defer func() {
		structs.IntermediateCertRenewInterval = origInterval
		structs.MinLeafCertTTL = origMinTTL
		ca.CertificateTimeDriftBuffer = origDriftBuffer
	}()

	// Vault backdates certs by 30s by default.
	ca.CertificateTimeDriftBuffer = 30 * time.Second
	structs.IntermediateCertRenewInterval = time.Millisecond
	structs.MinLeafCertTTL = time.Second
	require := require.New(t)

	testVault := ca.NewTestVaultServer(t)
	defer testVault.Stop()

	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.6.0"
		c.PrimaryDatacenter = "dc1"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "vault",
			Config: map[string]interface{}{
				"Address":             testVault.Addr,
				"Token":               testVault.RootToken,
				"RootPKIPath":         "pki-root/",
				"IntermediatePKIPath": "pki-intermediate/",
				"LeafCertTTL":         "1s",
				// The retry loop only retries for 7sec max and
				// the ttl needs to be below so that it
				// triggers definitely.
				"IntermediateCertTTL": "5s",
			},
		}
	})
	defer os.RemoveAll(dir1)
	defer func() {
		s1.Shutdown()
		s1.leaderRoutineManager.Wait()
	}()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// Capture the current root.
	var originalRoot *structs.CARoot
	{
		rootList, activeRoot, err := getTestRoots(s1, "dc1")
		require.NoError(err)
		require.Len(rootList.Roots, 1)
		originalRoot = activeRoot
	}

	// Get the original intermediate.
	waitForActiveCARoot(t, s1, originalRoot)
	provider, _ := getCAProviderWithLock(s1)
	intermediatePEM, err := provider.ActiveIntermediate()
	require.NoError(err)
	_, err = connect.ParseCert(intermediatePEM)
	require.NoError(err)

	// Wait for dc1's intermediate to be refreshed.
	// It is possible that test fails when the blocking query doesn't return.
	retry.Run(t, func(r *retry.R) {
		provider, _ = getCAProviderWithLock(s1)
		newIntermediatePEM, err := provider.ActiveIntermediate()
		r.Check(err)
		_, err = connect.ParseCert(intermediatePEM)
		r.Check(err)
		if newIntermediatePEM == intermediatePEM {
			r.Fatal("not a renewed intermediate")
		}
		intermediatePEM = newIntermediatePEM
	})
	require.NoError(err)

	// Get the root from dc1 and validate a chain of:
	// dc1 leaf -> dc1 intermediate -> dc1 root
	provider, caRoot := getCAProviderWithLock(s1)

	// Have the new intermediate sign a leaf cert and make sure the chain is correct.
	spiffeService := &connect.SpiffeIDService{
		Host:       "node1",
		Namespace:  "default",
		Datacenter: "dc1",
		Service:    "foo",
	}
	raw, _ := connect.TestCSR(t, spiffeService)

	leafCsr, err := connect.ParseCSR(raw)
	require.NoError(err)

	leafPEM, err := provider.Sign(leafCsr)
	require.NoError(err)

	cert, err := connect.ParseCert(leafPEM)
	require.NoError(err)

	// Check that the leaf signed by the new intermediate can be verified using the
	// returned cert chain (signed intermediate + remote root).
	intermediatePool := x509.NewCertPool()
	intermediatePool.AppendCertsFromPEM([]byte(intermediatePEM))
	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM([]byte(caRoot.RootCert))

	_, err = cert.Verify(x509.VerifyOptions{
		Intermediates: intermediatePool,
		Roots:         rootPool,
	})
	require.NoError(err)
}

func TestLeader_SecondaryCA_IntermediateRenew(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	// no parallel execution because we change globals
	origInterval := structs.IntermediateCertRenewInterval
	origMinTTL := structs.MinLeafCertTTL
	defer func() {
		structs.IntermediateCertRenewInterval = origInterval
		structs.MinLeafCertTTL = origMinTTL
	}()

	structs.IntermediateCertRenewInterval = time.Millisecond
	structs.MinLeafCertTTL = time.Second
	require := require.New(t)

	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.6.0"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "consul",
			Config: map[string]interface{}{
				"PrivateKey":  "",
				"RootCert":    "",
				"LeafCertTTL": "5s",
				// The retry loop only retries for 7sec max and
				// the ttl needs to be below so that it
				// triggers definitely.
				// Since certs are created so that they are
				// valid from 1minute in the past, we need to
				// account for that, otherwise it will be
				// expired immediately.
				"IntermediateCertTTL": time.Minute + (5 * time.Second),
			},
		}
	})
	defer os.RemoveAll(dir1)
	defer func() {
		s1.Shutdown()
		s1.leaderRoutineManager.Wait()
	}()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// dc2 as a secondary DC
	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.Build = "1.6.0"
	})
	defer os.RemoveAll(dir2)
	defer func() {
		s2.Shutdown()
		s2.leaderRoutineManager.Wait()
	}()

	// Create the WAN link
	joinWAN(t, s2, s1)
	testrpc.WaitForLeader(t, s2.RPC, "dc2")

	// Get the original intermediate
	// TODO: Wait for intermediate instead of wait for leader
	secondaryProvider, _ := getCAProviderWithLock(s2)
	intermediatePEM, err := secondaryProvider.ActiveIntermediate()
	require.NoError(err)
	cert, err := connect.ParseCert(intermediatePEM)
	require.NoError(err)
	currentCertSerialNumber := cert.SerialNumber
	currentCertAuthorityKeyId := cert.AuthorityKeyId

	// Capture the current root
	var originalRoot *structs.CARoot
	{
		rootList, activeRoot, err := getTestRoots(s1, "dc1")
		require.NoError(err)
		require.Len(rootList.Roots, 1)
		originalRoot = activeRoot
	}

	waitForActiveCARoot(t, s1, originalRoot)
	waitForActiveCARoot(t, s2, originalRoot)

	// Wait for dc2's intermediate to be refreshed.
	// It is possible that test fails when the blocking query doesn't return.
	// When https://github.com/hashicorp/consul/pull/3777 is merged
	// however, defaultQueryTime will be configurable and we con lower it
	// so that it returns for sure.
	retry.Run(t, func(r *retry.R) {
		secondaryProvider, _ = getCAProviderWithLock(s2)
		intermediatePEM, err = secondaryProvider.ActiveIntermediate()
		r.Check(err)
		cert, err := connect.ParseCert(intermediatePEM)
		r.Check(err)
		if cert.SerialNumber.Cmp(currentCertSerialNumber) == 0 || !reflect.DeepEqual(cert.AuthorityKeyId, currentCertAuthorityKeyId) {
			currentCertSerialNumber = cert.SerialNumber
			currentCertAuthorityKeyId = cert.AuthorityKeyId
			r.Fatal("not a renewed intermediate")
		}
	})
	require.NoError(err)

	// Get the root from dc1 and validate a chain of:
	// dc2 leaf -> dc2 intermediate -> dc1 root
	_, caRoot := getCAProviderWithLock(s1)

	// Have dc2 sign a leaf cert and make sure the chain is correct.
	spiffeService := &connect.SpiffeIDService{
		Host:       "node1",
		Namespace:  "default",
		Datacenter: "dc1",
		Service:    "foo",
	}
	raw, _ := connect.TestCSR(t, spiffeService)

	leafCsr, err := connect.ParseCSR(raw)
	require.NoError(err)

	leafPEM, err := secondaryProvider.Sign(leafCsr)
	require.NoError(err)

	cert, err = connect.ParseCert(leafPEM)
	require.NoError(err)

	// Check that the leaf signed by the new intermediate can be verified using the
	// returned cert chain (signed intermediate + remote root).
	intermediatePool := x509.NewCertPool()
	intermediatePool.AppendCertsFromPEM([]byte(intermediatePEM))
	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM([]byte(caRoot.RootCert))

	_, err = cert.Verify(x509.VerifyOptions{
		Intermediates: intermediatePool,
		Roots:         rootPool,
	})
	require.NoError(err)
}

func TestLeader_SecondaryCA_IntermediateRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	require := require.New(t)

	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.6.0"
		c.PrimaryDatacenter = "dc1"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// dc2 as a secondary DC
	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.Build = "1.6.0"
	})
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()

	// Create the WAN link
	joinWAN(t, s2, s1)
	testrpc.WaitForLeader(t, s2.RPC, "dc2")

	// Get the original intermediate
	secondaryProvider, _ := getCAProviderWithLock(s2)
	oldIntermediatePEM, err := secondaryProvider.ActiveIntermediate()
	require.NoError(err)
	require.NotEmpty(oldIntermediatePEM)

	// Capture the current root
	var originalRoot *structs.CARoot
	{
		rootList, activeRoot, err := getTestRoots(s1, "dc1")
		require.NoError(err)
		require.Len(rootList.Roots, 1)
		originalRoot = activeRoot
	}

	// Wait for current state to be reflected in both datacenters.
	testrpc.WaitForActiveCARoot(t, s1.RPC, "dc1", originalRoot)
	testrpc.WaitForActiveCARoot(t, s2.RPC, "dc2", originalRoot)

	// Update the provider config to use a new private key, which should
	// cause a rotation.
	_, newKey, err := connect.GeneratePrivateKey()
	require.NoError(err)
	newConfig := &structs.CAConfiguration{
		Provider: "consul",
		Config: map[string]interface{}{
			"PrivateKey":          newKey,
			"RootCert":            "",
			"IntermediateCertTTL": 72 * 24 * time.Hour,
		},
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		require.NoError(s1.RPC("ConnectCA.ConfigurationSet", args, &reply))
	}

	var updatedRoot *structs.CARoot
	{
		rootList, activeRoot, err := getTestRoots(s1, "dc1")
		require.NoError(err)
		require.Len(rootList.Roots, 2)
		updatedRoot = activeRoot
	}

	testrpc.WaitForActiveCARoot(t, s1.RPC, "dc1", updatedRoot)
	testrpc.WaitForActiveCARoot(t, s2.RPC, "dc2", updatedRoot)

	// Wait for dc2's intermediate to be refreshed.
	var intermediatePEM string
	retry.Run(t, func(r *retry.R) {
		intermediatePEM, err = secondaryProvider.ActiveIntermediate()
		r.Check(err)
		if intermediatePEM == oldIntermediatePEM {
			r.Fatal("not a new intermediate")
		}
	})
	require.NoError(err)

	// Verify the root lists have been rotated in each DC's state store.
	state1 := s1.fsm.State()
	_, primaryRoot, err := state1.CARootActive(nil)
	require.NoError(err)

	state2 := s2.fsm.State()
	_, roots2, err := state2.CARoots(nil)
	require.NoError(err)
	require.Equal(2, len(roots2))

	newRoot := roots2[0]
	oldRoot := roots2[1]
	if roots2[1].Active {
		newRoot = roots2[1]
		oldRoot = roots2[0]
	}
	require.False(oldRoot.Active)
	require.True(newRoot.Active)
	require.Equal(primaryRoot.ID, newRoot.ID)
	require.Equal(primaryRoot.RootCert, newRoot.RootCert)

	// Get the new root from dc1 and validate a chain of:
	// dc2 leaf -> dc2 intermediate -> dc1 root
	_, caRoot := getCAProviderWithLock(s1)

	// Have dc2 sign a leaf cert and make sure the chain is correct.
	spiffeService := &connect.SpiffeIDService{
		Host:       "node1",
		Namespace:  "default",
		Datacenter: "dc1",
		Service:    "foo",
	}
	raw, _ := connect.TestCSR(t, spiffeService)

	leafCsr, err := connect.ParseCSR(raw)
	require.NoError(err)

	leafPEM, err := secondaryProvider.Sign(leafCsr)
	require.NoError(err)

	cert, err := connect.ParseCert(leafPEM)
	require.NoError(err)

	// Check that the leaf signed by the new intermediate can be verified using the
	// returned cert chain (signed intermediate + remote root).
	intermediatePool := x509.NewCertPool()
	intermediatePool.AppendCertsFromPEM([]byte(intermediatePEM))
	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM([]byte(caRoot.RootCert))

	_, err = cert.Verify(x509.VerifyOptions{
		Intermediates: intermediatePool,
		Roots:         rootPool,
	})
	require.NoError(err)
}

func TestLeader_Vault_PrimaryCA_FixSigningKeyID_OnRestart(t *testing.T) {
	ca.SkipIfVaultNotPresent(t)

	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	testVault := ca.NewTestVaultServer(t)
	defer testVault.Stop()

	dir1pre, s1pre := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.6.0"
		c.PrimaryDatacenter = "dc1"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "vault",
			Config: map[string]interface{}{
				"Address":             testVault.Addr,
				"Token":               testVault.RootToken,
				"RootPKIPath":         "pki-root/",
				"IntermediatePKIPath": "pki-intermediate/",
			},
		}
	})
	defer os.RemoveAll(dir1pre)
	defer s1pre.Shutdown()

	testrpc.WaitForLeader(t, s1pre.RPC, "dc1")

	// Restore the pre-1.9.3/1.8.8/1.7.12 behavior of the SigningKeyID not being derived
	// from the intermediates in the primary (which only matters for provider=vault).
	var primaryRootSigningKeyID string
	{
		state := s1pre.fsm.State()

		// Get the highest index
		idx, activePrimaryRoot, err := state.CARootActive(nil)
		require.NoError(t, err)
		require.NotNil(t, activePrimaryRoot)

		rootCert, err := connect.ParseCert(activePrimaryRoot.RootCert)
		require.NoError(t, err)

		// Force this to be derived just from the root, not the intermediate.
		primaryRootSigningKeyID = connect.EncodeSigningKeyID(rootCert.SubjectKeyId)
		activePrimaryRoot.SigningKeyID = primaryRootSigningKeyID

		// Store the root cert in raft
		_, err = s1pre.raftApply(structs.ConnectCARequestType, &structs.CARequest{
			Op:    structs.CAOpSetRoots,
			Index: idx,
			Roots: []*structs.CARoot{activePrimaryRoot},
		})
		require.NoError(t, err)
	}

	// Shutdown s1pre and restart it to trigger the secondary CA init to correct
	// the SigningKeyID.
	s1pre.Shutdown()

	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.DataDir = s1pre.config.DataDir
		c.Datacenter = "dc1"
		c.PrimaryDatacenter = "dc1"
		c.NodeName = s1pre.config.NodeName
		c.NodeID = s1pre.config.NodeID
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// Retry since it will take some time to init the primary CA fully and there
	// isn't a super clean way to watch specifically until it's done than polling
	// the CA provider anyway.
	retry.Run(t, func(r *retry.R) {
		// verify that the root is now corrected
		provider, activeRoot := getCAProviderWithLock(s1)
		require.NotNil(r, provider)
		require.NotNil(r, activeRoot)

		activeIntermediate, err := provider.ActiveIntermediate()
		require.NoError(r, err)

		intermediateCert, err := connect.ParseCert(activeIntermediate)
		require.NoError(r, err)

		// Force this to be derived just from the root, not the intermediate.
		expect := connect.EncodeSigningKeyID(intermediateCert.SubjectKeyId)

		// The in-memory representation was saw the correction via a setCAProvider call.
		require.Equal(r, expect, activeRoot.SigningKeyID)

		// The state store saw the correction, too.
		_, activePrimaryRoot, err := s1.fsm.State().CARootActive(nil)
		require.NoError(r, err)
		require.NotNil(r, activePrimaryRoot)
		require.Equal(r, expect, activePrimaryRoot.SigningKeyID)
	})
}

func TestLeader_SecondaryCA_FixSigningKeyID_via_IntermediateRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.6.0"
		c.PrimaryDatacenter = "dc1"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// dc2 as a secondary DC
	dir2pre, s2pre := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.Build = "1.6.0"
	})
	defer os.RemoveAll(dir2pre)
	defer s2pre.Shutdown()

	// Create the WAN link
	joinWAN(t, s2pre, s1)
	testrpc.WaitForLeader(t, s2pre.RPC, "dc2")

	// Restore the pre-1.6.1 behavior of the SigningKeyID not being derived
	// from the intermediates.
	var secondaryRootSigningKeyID string
	{
		state := s2pre.fsm.State()

		// Get the highest index
		idx, activeSecondaryRoot, err := state.CARootActive(nil)
		require.NoError(t, err)
		require.NotNil(t, activeSecondaryRoot)

		rootCert, err := connect.ParseCert(activeSecondaryRoot.RootCert)
		require.NoError(t, err)

		// Force this to be derived just from the root, not the intermediate.
		secondaryRootSigningKeyID = connect.EncodeSigningKeyID(rootCert.SubjectKeyId)
		activeSecondaryRoot.SigningKeyID = secondaryRootSigningKeyID

		// Store the root cert in raft
		_, err = s2pre.raftApply(structs.ConnectCARequestType, &structs.CARequest{
			Op:    structs.CAOpSetRoots,
			Index: idx,
			Roots: []*structs.CARoot{activeSecondaryRoot},
		})
		require.NoError(t, err)
	}

	// Shutdown s2pre and restart it to trigger the secondary CA init to correct
	// the SigningKeyID.
	s2pre.Shutdown()

	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.DataDir = s2pre.config.DataDir
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.NodeName = s2pre.config.NodeName
		c.NodeID = s2pre.config.NodeID
	})
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()

	testrpc.WaitForLeader(t, s2.RPC, "dc2")

	// Retry since it will take some time to init the secondary CA fully and there
	// isn't a super clean way to watch specifically until it's done than polling
	// the CA provider anyway.
	retry.Run(t, func(r *retry.R) {
		// verify that the root is now corrected
		provider, activeRoot := getCAProviderWithLock(s2)
		require.NotNil(r, provider)
		require.NotNil(r, activeRoot)

		activeIntermediate, err := provider.ActiveIntermediate()
		require.NoError(r, err)

		intermediateCert, err := connect.ParseCert(activeIntermediate)
		require.NoError(r, err)

		// Force this to be derived just from the root, not the intermediate.
		expect := connect.EncodeSigningKeyID(intermediateCert.SubjectKeyId)

		// The in-memory representation was saw the correction via a setCAProvider call.
		require.Equal(r, expect, activeRoot.SigningKeyID)

		// The state store saw the correction, too.
		_, activeSecondaryRoot, err := s2.fsm.State().CARootActive(nil)
		require.NoError(r, err)
		require.NotNil(r, activeSecondaryRoot)
		require.Equal(r, expect, activeSecondaryRoot.SigningKeyID)
	})
}

func TestLeader_SecondaryCA_TransitionFromPrimary(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	// Initialize dc1 as the primary DC
	id1, err := uuid.GenerateUUID()
	require.NoError(t, err)
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.PrimaryDatacenter = "dc1"
		c.CAConfig.ClusterID = id1
		c.Build = "1.6.0"
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// dc2 as a primary DC initially
	id2, err := uuid.GenerateUUID()
	require.NoError(t, err)
	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc2"
		c.CAConfig.ClusterID = id2
		c.Build = "1.6.0"
	})
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()

	// Get the initial (primary) roots state for the secondary
	testrpc.WaitForLeader(t, s2.RPC, "dc2")
	args := structs.DCSpecificRequest{Datacenter: "dc2"}
	var dc2PrimaryRoots structs.IndexedCARoots
	require.NoError(t, s2.RPC("ConnectCA.Roots", &args, &dc2PrimaryRoots))
	require.Len(t, dc2PrimaryRoots.Roots, 1)

	// Shutdown s2 and restart it with the dc1 as the primary
	s2.Shutdown()
	dir3, s3 := testServerWithConfig(t, func(c *Config) {
		c.DataDir = s2.config.DataDir
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.NodeName = s2.config.NodeName
		c.NodeID = s2.config.NodeID
	})
	defer os.RemoveAll(dir3)
	defer s3.Shutdown()

	// Create the WAN link
	joinWAN(t, s3, s1)
	testrpc.WaitForLeader(t, s3.RPC, "dc2")

	// Verify the secondary has migrated its TrustDomain and added the new primary's root.
	retry.Run(t, func(r *retry.R) {
		args = structs.DCSpecificRequest{Datacenter: "dc1"}
		var dc1Roots structs.IndexedCARoots
		require.NoError(r, s1.RPC("ConnectCA.Roots", &args, &dc1Roots))
		require.Len(r, dc1Roots.Roots, 1)

		args = structs.DCSpecificRequest{Datacenter: "dc2"}
		var dc2SecondaryRoots structs.IndexedCARoots
		require.NoError(r, s3.RPC("ConnectCA.Roots", &args, &dc2SecondaryRoots))

		// dc2's TrustDomain should have changed to the primary's
		require.Equal(r, dc2SecondaryRoots.TrustDomain, dc1Roots.TrustDomain)
		require.NotEqual(r, dc2SecondaryRoots.TrustDomain, dc2PrimaryRoots.TrustDomain)

		// Both roots should be present and correct
		require.Len(r, dc2SecondaryRoots.Roots, 2)
		var oldSecondaryRoot *structs.CARoot
		var newSecondaryRoot *structs.CARoot
		if dc2SecondaryRoots.Roots[0].ID == dc2PrimaryRoots.Roots[0].ID {
			oldSecondaryRoot = dc2SecondaryRoots.Roots[0]
			newSecondaryRoot = dc2SecondaryRoots.Roots[1]
		} else {
			oldSecondaryRoot = dc2SecondaryRoots.Roots[1]
			newSecondaryRoot = dc2SecondaryRoots.Roots[0]
		}

		// The old root should have its TrustDomain filled in as the old domain.
		require.Equal(r, oldSecondaryRoot.ExternalTrustDomain, strings.TrimSuffix(dc2PrimaryRoots.TrustDomain, ".consul"))

		require.Equal(r, oldSecondaryRoot.ID, dc2PrimaryRoots.Roots[0].ID)
		require.Equal(r, oldSecondaryRoot.RootCert, dc2PrimaryRoots.Roots[0].RootCert)
		require.Equal(r, newSecondaryRoot.ID, dc1Roots.Roots[0].ID)
		require.Equal(r, newSecondaryRoot.RootCert, dc1Roots.Roots[0].RootCert)
	})
}

func TestLeader_SecondaryCA_UpgradeBeforePrimary(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	// Initialize dc1 as the primary DC
	dir1, s1 := testServerWithConfig(t, func(c *Config) {
		c.PrimaryDatacenter = "dc1"
		c.Build = "1.3.0"
		c.MaxQueryTime = 500 * time.Millisecond
	})
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// dc2 as a secondary DC
	dir2, s2 := testServerWithConfig(t, func(c *Config) {
		c.Datacenter = "dc2"
		c.PrimaryDatacenter = "dc1"
		c.Build = "1.6.0"
		c.MaxQueryTime = 500 * time.Millisecond
	})
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()

	// Create the WAN link
	joinWAN(t, s2, s1)
	testrpc.WaitForLeader(t, s2.RPC, "dc2")

	// ensure all the CA initialization stuff would have already been done
	// this is necessary to ensure that not only has a leader been elected
	// but that it has also finished its establishLeadership call
	retry.Run(t, func(r *retry.R) {
		require.True(r, s1.isReadyForConsistentReads())
		require.True(r, s2.isReadyForConsistentReads())
	})

	// Verify the primary has a root (we faked its version too low but since its the primary it ignores any version checks)
	retry.Run(t, func(r *retry.R) {
		state1 := s1.fsm.State()
		_, roots1, err := state1.CARoots(nil)
		require.NoError(r, err)
		require.Len(r, roots1, 1)
	})

	// Verify the secondary does not have a root - defers initialization until the primary has been upgraded.
	state2 := s2.fsm.State()
	_, roots2, err := state2.CARoots(nil)
	require.NoError(t, err)
	require.Empty(t, roots2)

	// Update the version on the fly so s2 kicks off the secondary DC transition.
	tags := s1.config.SerfWANConfig.Tags
	tags["build"] = "1.6.0"
	s1.serfWAN.SetTags(tags)

	// Wait for the secondary transition to happen and then verify the secondary DC
	// has both roots present.
	secondaryProvider, _ := getCAProviderWithLock(s2)
	retry.Run(t, func(r *retry.R) {
		state1 := s1.fsm.State()
		_, roots1, err := state1.CARoots(nil)
		require.NoError(r, err)
		require.Len(r, roots1, 1)

		state2 := s2.fsm.State()
		_, roots2, err := state2.CARoots(nil)
		require.NoError(r, err)
		require.Len(r, roots2, 1)

		// ensure the roots are the same
		require.Equal(r, roots1[0].ID, roots2[0].ID)
		require.Equal(r, roots1[0].RootCert, roots2[0].RootCert)

		inter, err := secondaryProvider.ActiveIntermediate()
		require.NoError(r, err)
		require.NotEmpty(r, inter, "should have valid intermediate")
	})

	_, caRoot := getCAProviderWithLock(s1)
	intermediatePEM, err := secondaryProvider.ActiveIntermediate()
	require.NoError(t, err)

	// Have dc2 sign a leaf cert and make sure the chain is correct.
	spiffeService := &connect.SpiffeIDService{
		Host:       "node1",
		Namespace:  "default",
		Datacenter: "dc1",
		Service:    "foo",
	}
	raw, _ := connect.TestCSR(t, spiffeService)

	leafCsr, err := connect.ParseCSR(raw)
	require.NoError(t, err)

	leafPEM, err := secondaryProvider.Sign(leafCsr)
	require.NoError(t, err)

	cert, err := connect.ParseCert(leafPEM)
	require.NoError(t, err)

	// Check that the leaf signed by the new cert can be verified using the
	// returned cert chain (signed intermediate + remote root).
	intermediatePool := x509.NewCertPool()
	intermediatePool.AppendCertsFromPEM([]byte(intermediatePEM))
	rootPool := x509.NewCertPool()
	rootPool.AppendCertsFromPEM([]byte(caRoot.RootCert))

	_, err = cert.Verify(x509.VerifyOptions{
		Intermediates: intermediatePool,
		Roots:         rootPool,
	})
	require.NoError(t, err)
}

func getTestRoots(s *Server, datacenter string) (*structs.IndexedCARoots, *structs.CARoot, error) {
	rootReq := &structs.DCSpecificRequest{
		Datacenter: datacenter,
	}
	var rootList structs.IndexedCARoots
	if err := s.RPC("ConnectCA.Roots", rootReq, &rootList); err != nil {
		return nil, nil, err
	}

	var active *structs.CARoot
	for _, root := range rootList.Roots {
		if root.Active {
			active = root
			break
		}
	}

	return &rootList, active, nil
}

func TestLeader_CARootPruning(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	// Can not use t.Parallel(), because this modifies a global.
	origPruneInterval := caRootPruneInterval
	caRootPruneInterval = 200 * time.Millisecond
	t.Cleanup(func() {
		// Reset the value of the global prune interval so that it doesn't affect other tests
		caRootPruneInterval = origPruneInterval
	})

	require := require.New(t)
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	testrpc.WaitForTestAgent(t, s1.RPC, "dc1")

	// Get the current root
	rootReq := &structs.DCSpecificRequest{
		Datacenter: "dc1",
	}
	var rootList structs.IndexedCARoots
	require.Nil(msgpackrpc.CallWithCodec(codec, "ConnectCA.Roots", rootReq, &rootList))
	require.Len(rootList.Roots, 1)
	oldRoot := rootList.Roots[0]

	// Update the provider config to use a new private key, which should
	// cause a rotation.
	_, newKey, err := connect.GeneratePrivateKey()
	require.NoError(err)
	newConfig := &structs.CAConfiguration{
		Provider: "consul",
		Config: map[string]interface{}{
			"LeafCertTTL":  "500ms",
			"PrivateKey":   newKey,
			"RootCert":     "",
			"SkipValidate": true,
		},
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		require.NoError(msgpackrpc.CallWithCodec(codec, "ConnectCA.ConfigurationSet", args, &reply))
	}

	// Should have 2 roots now.
	_, roots, err := s1.fsm.State().CARoots(nil)
	require.NoError(err)
	require.Len(roots, 2)

	time.Sleep(2 * time.Second)

	// Now the old root should be pruned.
	_, roots, err = s1.fsm.State().CARoots(nil)
	require.NoError(err)
	require.Len(roots, 1)
	require.True(roots[0].Active)
	require.NotEqual(roots[0].ID, oldRoot.ID)
}

func TestLeader_PersistIntermediateCAs(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	t.Parallel()

	require := require.New(t)
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	dir2, s2 := testServerDCBootstrap(t, "dc1", false)
	defer os.RemoveAll(dir2)
	defer s2.Shutdown()

	dir3, s3 := testServerDCBootstrap(t, "dc1", false)
	defer os.RemoveAll(dir3)
	defer s3.Shutdown()

	joinLAN(t, s2, s1)
	joinLAN(t, s3, s1)

	testrpc.WaitForLeader(t, s1.RPC, "dc1")

	// Get the current root
	rootReq := &structs.DCSpecificRequest{
		Datacenter: "dc1",
	}
	var rootList structs.IndexedCARoots
	require.Nil(msgpackrpc.CallWithCodec(codec, "ConnectCA.Roots", rootReq, &rootList))
	require.Len(rootList.Roots, 1)

	// Update the provider config to use a new private key, which should
	// cause a rotation.
	_, newKey, err := connect.GeneratePrivateKey()
	require.NoError(err)
	newConfig := &structs.CAConfiguration{
		Provider: "consul",
		Config: map[string]interface{}{
			"PrivateKey": newKey,
			"RootCert":   "",
		},
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		require.NoError(msgpackrpc.CallWithCodec(codec, "ConnectCA.ConfigurationSet", args, &reply))
	}

	// Get the active root before leader change.
	_, root := getCAProviderWithLock(s1)
	require.Len(root.IntermediateCerts, 1)

	// Force a leader change and make sure the root CA values are preserved.
	s1.Leave()
	s1.Shutdown()

	retry.Run(t, func(r *retry.R) {
		var leader *Server
		for _, s := range []*Server{s2, s3} {
			if s.IsLeader() {
				leader = s
				break
			}
		}
		if leader == nil {
			r.Fatal("no leader")
		}

		_, newLeaderRoot := getCAProviderWithLock(leader)
		if !reflect.DeepEqual(newLeaderRoot, root) {
			r.Fatalf("got %v, want %v", newLeaderRoot, root)
		}
	})
}

func TestLeader_ParseCARoot(t *testing.T) {
	type test struct {
		name             string
		pem              string
		wantSerial       uint64
		wantSigningKeyID string
		wantKeyType      string
		wantKeyBits      int
		wantErr          bool
	}
	// Test certs generated with
	//   go run connect/certgen/certgen.go -out-dir /tmp/connect-certs -key-type ec -key-bits 384
	// for various key types. This does limit the exposure to formats that might
	// exist in external certificates which can be used as Connect CAs.
	// Specifically many other certs will have serial numbers that don't fit into
	// 64 bits but for reasons we truncate down to 64 bits which means our
	// `SerialNumber` will not match the one reported by openssl. We should
	// probably fix that at some point as it seems like a big footgun but it would
	// be a breaking API change to change the type to not be a JSON number and
	// JSON numbers don't even support the full range of a uint64...
	tests := []test{
		{"no cert", "", 0, "", "", 0, true},
		{
			name: "default cert",
			// Watchout for indentations they will break PEM format
			pem: readTestData(t, "cert-with-ec-256-key.pem"),
			// Based on `openssl x509 -noout -text` report from the cert
			wantSerial:       8341954965092507701,
			wantSigningKeyID: "97:4D:17:81:64:F8:B4:AF:05:E8:6C:79:C5:40:3B:0E:3E:8B:C0:AE:38:51:54:8A:2F:05:DB:E3:E8:E4:24:EC",
			wantKeyType:      "ec",
			wantKeyBits:      256,
			wantErr:          false,
		},
		{
			name: "ec 384 cert",
			// Watchout for indentations they will break PEM format
			pem: readTestData(t, "cert-with-ec-384-key.pem"),
			// Based on `openssl x509 -noout -text` report from the cert
			wantSerial:       2935109425518279965,
			wantSigningKeyID: "0B:A0:88:9B:DC:95:31:51:2E:3D:D4:F9:42:D0:6A:A0:62:46:82:D2:7C:22:E7:29:A9:AA:E8:A5:8C:CF:C7:42",
			wantKeyType:      "ec",
			wantKeyBits:      384,
			wantErr:          false,
		},
		{
			name: "rsa 4096 cert",
			// Watchout for indentations they will break PEM format
			pem: readTestData(t, "cert-with-rsa-4096-key.pem"),
			// Based on `openssl x509 -noout -text` report from the cert
			wantSerial:       5186695743100577491,
			wantSigningKeyID: "92:FA:CC:97:57:1E:31:84:A2:33:DD:9B:6A:A8:7C:FC:BE:E2:94:CA:AC:B3:33:17:39:3B:B8:67:9B:DC:C1:08",
			wantKeyType:      "rsa",
			wantKeyBits:      4096,
			wantErr:          false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			root, err := parseCARoot(tt.pem, "consul", "cluster")
			if tt.wantErr {
				require.Error(err)
				return
			}
			require.NoError(err)
			require.Equal(tt.wantSerial, root.SerialNumber)
			require.Equal(strings.ToLower(tt.wantSigningKeyID), root.SigningKeyID)
			require.Equal(tt.wantKeyType, root.PrivateKeyType)
			require.Equal(tt.wantKeyBits, root.PrivateKeyBits)
		})
	}
}

func readTestData(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	bs, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("failed reading fixture file %s: %s", name, err)
	}
	return string(bs)
}

func TestLeader_lessThanHalfTimePassed(t *testing.T) {
	now := time.Now()
	require.False(t, lessThanHalfTimePassed(now, now.Add(-10*time.Second), now.Add(-5*time.Second)))
	require.False(t, lessThanHalfTimePassed(now, now.Add(-10*time.Second), now))
	require.False(t, lessThanHalfTimePassed(now, now.Add(-10*time.Second), now.Add(5*time.Second)))
	require.False(t, lessThanHalfTimePassed(now, now.Add(-10*time.Second), now.Add(10*time.Second)))

	require.True(t, lessThanHalfTimePassed(now, now.Add(-10*time.Second), now.Add(20*time.Second)))
}

func TestLeader_retryLoopBackoffHandleSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("too slow for testing.Short")
	}

	type test struct {
		desc     string
		loopFn   func() error
		abort    bool
		timedOut bool
	}
	success := func() error {
		return nil
	}
	failure := func() error {
		return fmt.Errorf("test error")
	}
	tests := []test{
		{"loop without error and no abortOnSuccess keeps running", success, false, true},
		{"loop with error and no abortOnSuccess keeps running", failure, false, true},
		{"loop without error and abortOnSuccess is stopped", success, true, false},
		{"loop with error and abortOnSuccess keeps running", failure, true, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			retryLoopBackoffHandleSuccess(ctx, tc.loopFn, func(_ error) {}, tc.abort)
			select {
			case <-ctx.Done():
				if !tc.timedOut {
					t.Fatal("should not have timed out")
				}
			default:
				if tc.timedOut {
					t.Fatal("should have timed out")
				}
			}
		})
	}
}

func TestLeader_Vault_BadCAConfigShouldntPreventLeaderEstablishment(t *testing.T) {
	ca.SkipIfVaultNotPresent(t)

	testVault := ca.NewTestVaultServer(t)
	defer testVault.Stop()

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.9.1"
		c.PrimaryDatacenter = "dc1"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "vault",
			Config: map[string]interface{}{
				"Address":             testVault.Addr,
				"Token":               "not-the-root",
				"RootPKIPath":         "pki-root/",
				"IntermediatePKIPath": "pki-intermediate/",
			},
		}
	})
	defer s1.Shutdown()

	waitForLeaderEstablishment(t, s1)

	rootsList, activeRoot, err := getTestRoots(s1, "dc1")
	require.NoError(t, err)
	require.Empty(t, rootsList.Roots)
	require.Nil(t, activeRoot)

	// Now that the leader is up and we have verified that there are no roots / CA init failed,
	// verify that we can reconfigure away from the bad configuration.
	newConfig := &structs.CAConfiguration{
		Provider: "vault",
		Config: map[string]interface{}{
			"Address":             testVault.Addr,
			"Token":               testVault.RootToken,
			"RootPKIPath":         "pki-root/",
			"IntermediatePKIPath": "pki-intermediate/",
		},
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		retry.Run(t, func(r *retry.R) {
			require.NoError(r, s1.RPC("ConnectCA.ConfigurationSet", args, &reply))
		})
	}

	rootsList, activeRoot, err = getTestRoots(s1, "dc1")
	require.NoError(t, err)
	require.NotEmpty(t, rootsList.Roots)
	require.NotNil(t, activeRoot)
}

func TestLeader_Consul_BadCAConfigShouldntPreventLeaderEstablishment(t *testing.T) {
	ca.SkipIfVaultNotPresent(t)

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.9.1"
		c.PrimaryDatacenter = "dc1"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "consul",
			Config: map[string]interface{}{
				"RootCert": "garbage",
			},
		}
	})
	defer s1.Shutdown()

	waitForLeaderEstablishment(t, s1)

	rootsList, activeRoot, err := getTestRoots(s1, "dc1")
	require.NoError(t, err)
	require.Empty(t, rootsList.Roots)
	require.Nil(t, activeRoot)

	newConfig := &structs.CAConfiguration{
		Provider: "consul",
		Config:   map[string]interface{}{},
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		retry.Run(t, func(r *retry.R) {
			require.NoError(r, s1.RPC("ConnectCA.ConfigurationSet", args, &reply))
		})
	}

	rootsList, activeRoot, err = getTestRoots(s1, "dc1")
	require.NoError(t, err)
	require.NotEmpty(t, rootsList.Roots)
	require.NotNil(t, activeRoot)
}

func TestLeader_Consul_ForceWithoutCrossSigning(t *testing.T) {
	require := require.New(t)
	dir1, s1 := testServer(t)
	defer os.RemoveAll(dir1)
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	waitForLeaderEstablishment(t, s1)

	// Get the current root
	rootReq := &structs.DCSpecificRequest{
		Datacenter: "dc1",
	}
	var rootList structs.IndexedCARoots
	require.Nil(msgpackrpc.CallWithCodec(codec, "ConnectCA.Roots", rootReq, &rootList))
	require.Len(rootList.Roots, 1)
	oldRoot := rootList.Roots[0]

	// Update the provider config to use a new private key, which should
	// cause a rotation.
	_, newKey, err := connect.GeneratePrivateKey()
	require.NoError(err)
	newConfig := &structs.CAConfiguration{
		Provider: "consul",
		Config: map[string]interface{}{
			"LeafCertTTL":  "500ms",
			"PrivateKey":   newKey,
			"RootCert":     "",
			"SkipValidate": true,
		},
		ForceWithoutCrossSigning: true,
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		require.NoError(msgpackrpc.CallWithCodec(codec, "ConnectCA.ConfigurationSet", args, &reply))
	}

	// Old root should no longer be active.
	_, roots, err := s1.fsm.State().CARoots(nil)
	require.NoError(err)
	require.Len(roots, 2)
	for _, r := range roots {
		if r.ID == oldRoot.ID {
			require.False(r.Active)
		} else {
			require.True(r.Active)
		}
	}
}

func TestLeader_Vault_ForceWithoutCrossSigning(t *testing.T) {
	ca.SkipIfVaultNotPresent(t)

	require := require.New(t)
	testVault := ca.NewTestVaultServer(t)
	defer testVault.Stop()

	_, s1 := testServerWithConfig(t, func(c *Config) {
		c.Build = "1.9.1"
		c.PrimaryDatacenter = "dc1"
		c.CAConfig = &structs.CAConfiguration{
			Provider: "vault",
			Config: map[string]interface{}{
				"Address":             testVault.Addr,
				"Token":               testVault.RootToken,
				"RootPKIPath":         "pki-root/",
				"IntermediatePKIPath": "pki-intermediate/",
			},
		}
	})
	defer s1.Shutdown()
	codec := rpcClient(t, s1)
	defer codec.Close()

	waitForLeaderEstablishment(t, s1)

	// Get the current root
	rootReq := &structs.DCSpecificRequest{
		Datacenter: "dc1",
	}
	var rootList structs.IndexedCARoots
	require.Nil(msgpackrpc.CallWithCodec(codec, "ConnectCA.Roots", rootReq, &rootList))
	require.Len(rootList.Roots, 1)
	oldRoot := rootList.Roots[0]

	// Update the provider config to use a new PKI path, which should
	// cause a rotation.
	newConfig := &structs.CAConfiguration{
		Provider: "vault",
		Config: map[string]interface{}{
			"Address":             testVault.Addr,
			"Token":               testVault.RootToken,
			"RootPKIPath":         "pki-root-2/",
			"IntermediatePKIPath": "pki-intermediate/",
		},
		ForceWithoutCrossSigning: true,
	}
	{
		args := &structs.CARequest{
			Datacenter: "dc1",
			Config:     newConfig,
		}
		var reply interface{}

		require.NoError(msgpackrpc.CallWithCodec(codec, "ConnectCA.ConfigurationSet", args, &reply))
	}

	// Old root should no longer be active.
	_, roots, err := s1.fsm.State().CARoots(nil)
	require.NoError(err)
	require.Len(roots, 2)
	for _, r := range roots {
		if r.ID == oldRoot.ID {
			require.False(r.Active)
		} else {
			require.True(r.Active)
		}
	}
}
