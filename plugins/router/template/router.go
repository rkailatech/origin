package templaterouter

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"text/template"

	"github.com/golang/glog"

	routeapi "github.com/openshift/origin/pkg/route/api"
)

const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
	ProtocolTLS   = "tls"
)

const (
	routeFile       = "/var/lib/containers/router/routes.json"
	certDir         = "/var/lib/containers/router/certs/"
	caCertDir       = "/var/lib/containers/router/cacerts/"
	defaultCertName = "default"

	caCertPostfix   = "_ca"
	destCertPostfix = "_pod"
)

// templateRouter is a backend-agnostic router implementation
// that generates configuration files via a set of templates
// and manages the backend process with a reload script.
type templateRouter struct {
	templates        map[string]*template.Template
	reloadScriptPath string
	state            map[string]ServiceUnit
	certManager      certManager
	// defaultCertificate is a concatenated certificate(s), their keys, and their CAs that should be used by the underlying
	// implementation as the default certificate if no certificate is resolved by the normal matching mechanisms.  This is
	// usually a wildcard certificate for a cloud domain such as *.mypaas.com to allow applications to create app.mypaas.com
	// as secure routes without having to provide their own certificates
	defaultCertificate string
	// if the default certificate is populated then this will be filled in so it can be passed to the templates
	defaultCertificatePath string
}

// templateConfig is a subset of the templateRouter information that should be passed to the template for generating
// the correct configuration.
type templateData struct {
	//the routes
	State map[string]ServiceUnit
	//full path and file name to the default certificate
	DefaultCertificate string
}

func newTemplateRouter(templates map[string]*template.Template, reloadScriptPath, defaultCertificate string) (*templateRouter, error) {
	glog.Infof("Creating a new template router")
	router := &templateRouter{
		templates:              templates,
		reloadScriptPath:       reloadScriptPath,
		state:                  map[string]ServiceUnit{},
		certManager:            certManager{},
		defaultCertificate:     defaultCertificate,
		defaultCertificatePath: "",
	}
	if err := router.writeDefaultCert(); err != nil {
		return nil, err
	}
	glog.Infof("Reading any persisted state")
	if err := router.readState(); err != nil {
		return nil, err
	}
	glog.Infof("Performing initial commit")
	if err := router.Commit(); err != nil {
		return nil, err
	}
	return router, nil
}

// writeDefaultCert is called a single time during init to write out the default certificate
func (r *templateRouter) writeDefaultCert() error {
	if len(r.defaultCertificate) > 0 {
		glog.Infof("Writing default certificate to %s", certDir)
		err := r.certManager.writeCertificate(certDir, defaultCertName, []byte(r.defaultCertificate))
		if err == nil {
			r.defaultCertificatePath = fmt.Sprintf("%s%s.pem", certDir, defaultCertName)
		}
		return err
	}
	return nil
}

func (r *templateRouter) readState() error {
	dat, err := ioutil.ReadFile(routeFile)
	// XXX: rework
	if err != nil {
		r.state = make(map[string]ServiceUnit)
		return nil
	}

	return json.Unmarshal(dat, &r.state)
}

// Commit refreshes the backend and persists the router state.
func (r *templateRouter) Commit() error {
	glog.V(4).Info("Commiting router changes")

	if err := r.writeState(); err != nil {
		return err
	}

	if err := r.writeConfig(); err != nil {
		return err
	}

	if err := r.reloadRouter(); err != nil {
		return err
	}

	return nil
}

// writeState writes the state of this router to disk.
func (r *templateRouter) writeState() error {
	dat, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		glog.Errorf("Failed to marshal route table: %v", err)
		return err
	}
	err = ioutil.WriteFile(routeFile, dat, 0644)
	if err != nil {
		glog.Errorf("Failed to write route table: %v", err)
		return err
	}

	return nil
}

// writeConfig writes the config to disk
func (r *templateRouter) writeConfig() error {
	//write out any certificate files that don't exist
	for _, serviceUnit := range r.state {
		for _, cfg := range serviceUnit.ServiceAliasConfigs {
			err := r.writeCertificates(&cfg)
			if err != nil {
				glog.Errorf("Error writing certificates for %s: %v", serviceUnit.Name, err)
				return err
			}
		}
	}

	for path, template := range r.templates {
		file, err := os.Create(path)
		if err != nil {
			glog.Errorf("Error creating config file %v: %v", path, err)
			return err
		}

		err = template.Execute(file, templateData{r.state, r.defaultCertificatePath})
		if err != nil {
			glog.Errorf("Error executing template for file %v: %v", path, err)
			return err
		}

		file.Close()
	}

	return nil
}

// writeCertificates attempts to write certificates only if the cfg requires it see shouldWriteCerts
// for details
func (r *templateRouter) writeCertificates(cfg *ServiceAliasConfig) error {
	if r.shouldWriteCerts(cfg) {
		//TODO: better way so this doesn't need to create lots of files every time state is written, probably too expensive
		return r.certManager.writeCertificatesForConfig(cfg)
	}
	return nil
}

// reloadRouter executes the router's reload script.
func (r *templateRouter) reloadRouter() error {
	cmd := exec.Command(r.reloadScriptPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		glog.Errorf("Error reloading router: %v\n Reload output: %v", err, string(out))
	}
	return err
}

// CreateServiceUnit creates a new service named with the given id.
func (r *templateRouter) CreateServiceUnit(id string) {
	service := ServiceUnit{
		Name:                id,
		ServiceAliasConfigs: make(map[string]ServiceAliasConfig),
		EndpointTable:       make(map[string]Endpoint),
	}

	r.state[id] = service
}

// FindServiceUnit finds the service with the given id.
func (r *templateRouter) FindServiceUnit(id string) (v ServiceUnit, ok bool) {
	v, ok = r.state[id]
	return
}

// DeleteServiceUnit deletes the service with the given id.
func (r *templateRouter) DeleteServiceUnit(id string) {
	delete(r.state, id)
}

// DeleteEndpoints deletes the endpoints for the service with the given id.
func (r *templateRouter) DeleteEndpoints(id string) {
	service, ok := r.FindServiceUnit(id)
	if !ok {
		return
	}
	service.EndpointTable = make(map[string]Endpoint)

	r.state[id] = service
}

// routeKey generates route key in form of Namespace-Name.  This is NOT the normal key structure of ns/name because
// it is not safe to use / in names of router config files.  This allows templates to use this key without having
// to create (or provide) a separate method
func (r *templateRouter) routeKey(route *routeapi.Route) string {
	return fmt.Sprintf("%s-%s", route.Namespace, route.Name)
}

// AddRoute adds a route for the given id
func (r *templateRouter) AddRoute(id string, route *routeapi.Route) {
	frontend, _ := r.FindServiceUnit(id)

	backendKey := r.routeKey(route)

	config := ServiceAliasConfig{
		Host: route.Host,
		Path: route.Path,
	}

	if route.TLS != nil && len(route.TLS.Termination) > 0 {
		config.TLSTermination = route.TLS.Termination

		if route.TLS.Termination != routeapi.TLSTerminationPassthrough {
			if config.Certificates == nil {
				config.Certificates = make(map[string]Certificate)
			}

			cert := Certificate{
				ID:         route.Host,
				Contents:   route.TLS.Certificate,
				PrivateKey: route.TLS.Key,
			}

			config.Certificates[cert.ID] = cert

			if len(route.TLS.CACertificate) > 0 {
				caCert := Certificate{
					ID:       route.Host + caCertPostfix,
					Contents: route.TLS.CACertificate,
				}

				config.Certificates[caCert.ID] = caCert
			}

			if len(route.TLS.DestinationCACertificate) > 0 {
				destCert := Certificate{
					ID:       route.Host + destCertPostfix,
					Contents: route.TLS.DestinationCACertificate,
				}

				config.Certificates[destCert.ID] = destCert
			}
		}
	}

	//create or replace
	frontend.ServiceAliasConfigs[backendKey] = config
	r.state[id] = frontend
}

// RemoveRoute removes the given route for the given id.
func (r *templateRouter) RemoveRoute(id string, route *routeapi.Route) {
	_, ok := r.state[id]

	if !ok {
		return
	}

	delete(r.state[id].ServiceAliasConfigs, r.routeKey(route))
}

// AddEndpoints adds new Endpoints for the given id.
func (r *templateRouter) AddEndpoints(id string, endpoints []Endpoint) {
	frontend, _ := r.FindServiceUnit(id)

	//only add if it doesn't already exist
	for _, ep := range endpoints {
		if _, ok := frontend.EndpointTable[ep.ID]; !ok {
			newEndpoint := Endpoint{ep.ID, ep.IP, ep.Port}
			frontend.EndpointTable[ep.ID] = newEndpoint
		}
	}

	r.state[id] = frontend
}

func cmpStrSlices(first []string, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for _, fi := range first {
		found := false
		for _, si := range second {
			if fi == si {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// shouldWriteCerts determines if the router should ask the cert manager to write out certificates
// it will return true if a route is edge or reencrypt and it has all the required (host/key) certificates
// defined.  If the route does not have the certificates defined it will log an info message if the
// router is configured with a default certificate and assume the route is meant to be a wildcard.  Otherwise
// it will log a warning.  The route will still be written but users may receive browser errors
// for a host/cert mismatch
func (r *templateRouter) shouldWriteCerts(cfg *ServiceAliasConfig) bool {
	if cfg.Certificates == nil {
		return false
	}

	if cfg.TLSTermination == routeapi.TLSTerminationEdge || cfg.TLSTermination == routeapi.TLSTerminationReencrypt {
		if hasRequiredEdgeCerts(cfg) {
			return true
		} else {
			msg := fmt.Sprintf("a %s terminated route with host %s does not have the required certificates.  The route will still be created but no certificates will be written",
				cfg.TLSTermination, cfg.Host)
			// if a default cert is configured we'll assume it is meant to be a wildcard and only log info
			// otherwise we'll consider this a warning
			if len(r.defaultCertificate) > 0 {
				glog.V(4).Info(msg)
			} else {
				glog.Warning(msg)
			}
			return false
		}
	}
	return false
}

// hasRequiredEdgeCerts ensures that at least a host certificate and key are provided.
// a ca cert is not required because it may be something that is in the root cert chain
func hasRequiredEdgeCerts(cfg *ServiceAliasConfig) bool {
	hostCert, ok := cfg.Certificates[cfg.Host]
	if ok && len(hostCert.Contents) > 0 && len(hostCert.PrivateKey) > 0 {
		return true
	}
	return false
}
