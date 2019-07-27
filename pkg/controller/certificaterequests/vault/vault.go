/*
Copyright 2019 The Jetstack cert-manager contributors.

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

package vault

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"path"
	"strings"

	vault "github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/certutil"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	apiutil "github.com/jetstack/cert-manager/pkg/api/util"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	controllerpkg "github.com/jetstack/cert-manager/pkg/controller"
	"github.com/jetstack/cert-manager/pkg/controller/certificaterequests"
	"github.com/jetstack/cert-manager/pkg/issuer"
	logf "github.com/jetstack/cert-manager/pkg/logs"
	"github.com/jetstack/cert-manager/pkg/util/pki"
)

const (
	CRControllerName = "certificaterequests-issuer-vault"
)

type Vault struct {
	// used to record Events about resources to the API
	recorder record.EventRecorder

	issuerOptions controllerpkg.IssuerOptions
	secretsLister corelisters.SecretLister
	helper        issuer.Helper
}

func init() {
	// create certificate request controller for ca issuer
	controllerpkg.Register(CRControllerName, func(ctx *controllerpkg.Context) (controllerpkg.Interface, error) {
		vault := NewVault(ctx)

		controller := certificaterequests.New(apiutil.IssuerVault, vault)

		c, err := controllerpkg.New(ctx, CRControllerName, controller)
		if err != nil {
			return nil, err
		}

		return c.Run, nil
	})
}

func NewVault(ctx *controllerpkg.Context) *Vault {
	return &Vault{
		recorder:      ctx.Recorder,
		issuerOptions: ctx.IssuerOptions,
		secretsLister: ctx.KubeSharedInformerFactory.Core().V1().Secrets().Lister(),
		helper: issuer.NewHelper(
			ctx.SharedInformerFactory.Certmanager().V1alpha1().Issuers().Lister(),
			ctx.SharedInformerFactory.Certmanager().V1alpha1().ClusterIssuers().Lister(),
		),
	}
}

func (v *Vault) Sign(ctx context.Context, cr *v1alpha1.CertificateRequest) (*issuer.IssueResponse, error) {
	log := logf.FromContext(ctx, "sign")
	dbg := log.V(logf.DebugLevel)

	issuerObj, err := v.helper.GetGenericIssuer(cr.Spec.IssuerRef, cr.Namespace)
	if k8sErrors.IsNotFound(err) {
		// pending fail wait

		return nil, nil
	}
	if err != nil {
		// pending fail backoff
		return nil, err
	}

	csr, err := pki.DecodeX509CertificateRequestBytes(cr.Spec.CSRPEM)
	if err != nil {
		// hard fail
		return nil, nil
	}

	client, err := v.initVaultClient(cr, issuerObj)
	if err != nil {
		// hard fail?
		//return nil, err
		return nil, nil
	}

	ipSans := pki.IPAddressesToString(csr.IPAddresses)

	dbg.Info("Vault certificate request", "commonName", csr.Subject.CommonName,
		"altNames", csr.DNSNames, "ipSans", ipSans)

	parameters := map[string]string{
		"common_name":          csr.Subject.CommonName,
		"alt_names":            strings.Join(csr.DNSNames, ","),
		"ip_sans":              strings.Join(ipSans, ","),
		"ttl":                  cr.Spec.Duration.String(),
		"csr":                  string(cr.Spec.CSRPEM),
		"exclude_cn_from_sans": "true",
	}

	url := path.Join("/v1", issuerObj.GetSpec().Vault.Path)

	request := client.NewRequest("POST", url)

	err = request.SetJSONBody(parameters)
	if err != nil {
		// hard fail?
		//return nil, fmt.Errorf("error encoding Vault parameters: %s", err.Error())
		return nil, nil
	}

	resp, err := client.RawRequest(request)
	if err != nil {
		// pending fail?
		//return nil, nil, fmt.Errorf("error signing certificate in Vault: %s", err.Error())
		return nil, nil
	}

	defer resp.Body.Close()

	vaultResult := certutil.Secret{}
	resp.DecodeJSON(&vaultResult)
	if err != nil {
		// hard fail?
		//return nil, nil, fmt.Errorf("unable to decode JSON payload: %s", err.Error())
		return nil, nil
	}

	parsedBundle, err := certutil.ParsePKIMap(vaultResult.Data)
	if err != nil {
		// hard fail?
		//return nil, nil, fmt.Errorf("unable to parse certificate: %s", err.Error())
		return nil, nil
	}

	bundle, err := parsedBundle.ToCertBundle()
	if err != nil {
		// hard fail?
		//return nil, nil, fmt.Errorf("unable to convert certificate bundle to PEM bundle: %s", err.Error())
		return nil, nil
	}

	var caPem []byte = nil
	if len(bundle.CAChain) > 0 {
		caPem = []byte(bundle.CAChain[0])
	}

	return &issuer.IssueResponse{
		Certificate: []byte(bundle.ToPEMBundle()),
		CA:          caPem,
	}, nil
}

func (v *Vault) configureCertPool(cfg *vault.Config, issuerObj v1alpha1.GenericIssuer) error {
	certs := issuerObj.GetSpec().Vault.CABundle
	if len(certs) == 0 {
		return nil
	}

	caCertPool := x509.NewCertPool()
	ok := caCertPool.AppendCertsFromPEM(certs)
	if ok == false {
		return fmt.Errorf("error loading Vault CA bundle")
	}

	cfg.HttpClient.Transport.(*http.Transport).TLSClientConfig.RootCAs = caCertPool

	return nil
}

func (v *Vault) requestTokenWithAppRoleRef(cr *v1alpha1.CertificateRequest, client *vault.Client,
	appRole *v1alpha1.VaultAppRole) (string, error) {
	roleId, secretId, err := v.appRoleRef(cr, appRole)
	if err != nil {
		return "", fmt.Errorf("error reading Vault AppRole from secret: %s/%s: %s", appRole.SecretRef.Name, cr.Namespace, err.Error())
	}

	parameters := map[string]string{
		"role_id":   roleId,
		"secret_id": secretId,
	}

	authPath := appRole.Path
	if authPath == "" {
		authPath = "approle"
	}

	url := path.Join("/v1", "auth", authPath, "login")

	request := client.NewRequest("POST", url)

	err = request.SetJSONBody(parameters)
	if err != nil {
		return "", fmt.Errorf("error encoding Vault parameters: %s", err.Error())
	}

	resp, err := client.RawRequest(request)
	if err != nil {
		return "", fmt.Errorf("error logging in to Vault server: %s", err.Error())
	}

	defer resp.Body.Close()

	vaultResult := vault.Secret{}
	resp.DecodeJSON(&vaultResult)
	if err != nil {
		return "", fmt.Errorf("unable to decode JSON payload: %s", err.Error())
	}

	token, err := vaultResult.TokenID()
	if err != nil {
		return "", fmt.Errorf("unable to read token: %s", err.Error())
	}

	return token, nil
}

func (v *Vault) appRoleRef(cr *v1alpha1.CertificateRequest, appRole *v1alpha1.VaultAppRole) (roleId, secretId string, err error) {
	roleId = strings.TrimSpace(appRole.RoleId)

	secret, err := v.secretsLister.Secrets(cr.Namespace).Get(appRole.SecretRef.Name)
	if err != nil {
		return "", "", err
	}

	key := "secretId"
	if appRole.SecretRef.Key != "secretId" {
		key = appRole.SecretRef.Key
	}

	keyBytes, ok := secret.Data[key]
	if !ok {
		return "", "", fmt.Errorf("no data for %q in secret '%s/%s'", key, appRole.SecretRef.Name, cr.Namespace)
	}

	secretId = string(keyBytes)
	secretId = strings.TrimSpace(secretId)

	return roleId, secretId, nil
}

func (v *Vault) vaultTokenRef(name, namespace, key string) (string, error) {
	secret, err := v.secretsLister.Secrets(namespace).Get(name)
	if err != nil {
		return "", err
	}

	if key == "" {
		key = "token"
	}

	keyBytes, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("no data for %q in secret '%s/%s'", key, name, namespace)
	}

	token := string(keyBytes)
	token = strings.TrimSpace(token)

	return token, nil
}

func (v *Vault) initVaultClient(cr *v1alpha1.CertificateRequest, issuerObj v1alpha1.GenericIssuer) (*vault.Client, error) {
	vaultCfg := vault.DefaultConfig()
	vaultCfg.Address = issuerObj.GetSpec().Vault.Server
	err := v.configureCertPool(vaultCfg, issuerObj)
	if err != nil {
		return nil, err
	}

	client, err := vault.NewClient(vaultCfg)
	if err != nil {
		return nil, fmt.Errorf("error initializing Vault client: %s", err.Error())
	}

	tokenRef := issuerObj.GetSpec().Vault.Auth.TokenSecretRef
	if tokenRef.Name != "" {
		token, err := v.vaultTokenRef(tokenRef.Name, cr.Namespace, tokenRef.Key)
		if err != nil {
			return nil, fmt.Errorf("error reading Vault token from secret %s/%s: %s", cr.Namespace, tokenRef.Name, err.Error())
		}
		client.SetToken(token)

		return client, nil
	}

	appRole := issuerObj.GetSpec().Vault.Auth.AppRole
	if appRole.RoleId != "" {
		token, err := v.requestTokenWithAppRoleRef(cr, client, &appRole)
		if err != nil {
			return nil, fmt.Errorf("error reading Vault token from AppRole: %s", err.Error())
		}
		client.SetToken(token)

		return client, nil
	}

	return nil, fmt.Errorf("error initializing Vault client. tokenSecretRef or appRoleSecretRef not set")
}
