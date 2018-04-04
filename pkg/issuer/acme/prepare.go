package acme

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang/glog"
	"github.com/jetstack/cert-manager/pkg/apis/certmanager/v1alpha1"
	"github.com/jetstack/cert-manager/pkg/issuer/acme/client"
	"github.com/jetstack/cert-manager/third_party/crypto/acme"
)

const (
	successObtainedAuthorization = "ObtainAuthorization"
	reasonPresentChallenge       = "PresentChallenge"
	reasonSelfCheck              = "SelfCheck"
	errorGetACMEAccount          = "ErrGetACMEAccount"
	errorCheckAuthorization      = "ErrCheckAuthorization"
	errorObtainAuthorization     = "ErrObtainAuthorization"
	errorInvalidConfig           = "ErrInvalidConfig"

	messageObtainedAuthorization    = "Obtained authorization for domain %s"
	messagePresentChallenge         = "Presenting %s challenge for domain %s"
	messageSelfCheck                = "Performing self-check for domain %s"
	messageErrorGetACMEAccount      = "Error getting ACME account: "
	messageErrorCheckAuthorization  = "Error checking ACME domain validation: "
	messageErrorObtainAuthorization = "Error obtaining ACME domain authorization: "
	messageErrorMissingConfig       = "certificate.spec.acme must be specified"
)

// Prepare will ensure the issuer has been initialised and is ready to issue
// certificates for the domains listed on the Certificate resource.
//
// It will send the appropriate Letsencrypt authorizations, and complete
// challenge requests if neccessary.
func (a *Acme) Prepare(ctx context.Context, crt *v1alpha1.Certificate) error {
	if crt.Spec.ACME == nil {
		crt.UpdateStatusCondition(v1alpha1.CertificateConditionReady, v1alpha1.ConditionFalse, errorInvalidConfig, messageErrorMissingConfig)
		return fmt.Errorf(messageErrorMissingConfig)
	}

	glog.V(4).Infof("Getting ACME client")
	// obtain an ACME client
	cl, err := a.acmeClient()
	if err != nil {
		return err
	}

	// TODO: clean up old authorization attempts.
	// This is complex because the Certificate resource may have been updated
	// to no longer include information required to clean up the challenge
	// (for example, if a domain is removed from a certificate while an authz
	// is in progress). This means we need to store a list of challenges we are
	// currently presenting on the certificates status field, which contains
	// enough information to clean up the resource:
	//
	// http01: domain and the fact it is http01
	// dns01: domain, token, key and dns01 provider name

	order, err := a.getOrCreateOrder(ctx, cl, crt)
	if err != nil {
		return err
	}

	allAuthorizations, err := getAuthorizations(ctx, cl, order.Authorizations...)
	if err != nil {
		s := messageErrorCheckAuthorization + err.Error()
		crt.UpdateStatusCondition(v1alpha1.CertificateConditionReady, v1alpha1.ConditionFalse, errorCheckAuthorization, s)
		return errors.New(s)
	}

	failed, pending, valid := partitionAuthorizations(allAuthorizations...)
	glog.Infof("Authorizations for Certificate %q: %d failed, %d pending, %d valid", crt.Name, len(failed), len(pending), len(valid))
	toCleanup := append(failed, valid...)
	for _, auth := range toCleanup {
		err := a.cleanupAuthorization(ctx, cl, crt, auth)
		if err != nil {
			return err
		}
	}

	if len(failed) > 0 {
		glog.Infof("Found %d failed authorizations. Cleaning up pending authorizations and clearing order URL")
		// clear the order url to trigger a new order to be created
		crt.Status.ACMEStatus().Order.URL = ""
		// clean up pending authorizations
		for _, auth := range pending {
			err := a.cleanupAuthorization(ctx, cl, crt, auth)
			if err != nil {
				// TODO: clean up remaining authorizations if one fails
				return err
			}
		}
		// TODO: pretty-print the list of failed authorizations
		s := fmt.Sprintf("Error obtaining validations for domains %v", failed)
		crt.UpdateStatusCondition(v1alpha1.CertificateConditionReady, v1alpha1.ConditionFalse, errorCheckAuthorization, s)
		return errors.New(s)
	}

	// all validations have been obtained
	if len(pending) == 0 {
		glog.Infof("No more pending authorizations remaining - challenge verification complete")
		return nil
	}

	var failingSelfChecks []string
	for _, auth := range pending {
		selfCheckPassed, challenge, err := a.presentAuthorization(ctx, cl, crt, auth)
		if err != nil {
			return err
		}
		if selfCheckPassed {
			glog.Infof("Self check passed for domain %q", auth.Identifier.Value)
			err := a.acceptChallenge(ctx, cl, auth, challenge)
			if err != nil {
				return err
			}
		} else {
			glog.Infof("Self check failed for domain %q", auth.Identifier.Value)
			failingSelfChecks = append(failingSelfChecks, auth.Identifier.Value)
		}
	}

	if len(failingSelfChecks) > 0 {
		return fmt.Errorf("self check failed for domains: %v", failingSelfChecks)
	}

	return nil
}

// getOrCreateOrder will attempt to retrieve an existing order for a
// certificate using the status.acme.order.url field.
//
// - if it's not set, it will call createOrder and return
//
// - if it is set, and the order is not in an error state, it will be returned
//
// - if it is set, and the order is in an invalid state, an event will be
//   logged and createOrder will be called
//
// - if an error occurs obtaining the order, it will be returned
func (a *Acme) getOrCreateOrder(ctx context.Context, cl client.Interface, crt *v1alpha1.Certificate) (*acme.Order, error) {
	orderURL := crt.Status.ACMEStatus().Order.URL
	glog.Infof("Checking existing order URL %q", orderURL)
	var err error
	var order *acme.Order
	// if the existing order URL is blank, create a new order
	if orderURL == "" {
		glog.Infof("Existing order URL not set. Creating new order.")
		return a.createOrder(ctx, cl, crt)
	}

	glog.Infof("Requesting order details for %q from ACME server", crt.Name)
	order, err = cl.GetOrder(ctx, orderURL)

	if err != nil {
		glog.Infof("Error requesting existing order details for %q from ACME server: %v", crt.Name, err)
		return nil, err
	}

	if !orderIsValidForCertificate(order, crt) {
		glog.Infof("Existing order is not valid for requested DNS names. Creating new order.")
		return a.createOrder(ctx, cl, crt)
	}

	glog.Infof("Order %q status is %q", order.URL, order.Status)
	switch order.Status {
	// create a new order if the old one is invalid
	case acme.StatusDeactivated, acme.StatusInvalid, acme.StatusRevoked:
		// TODO: log an event
		glog.Infof("Existing order is in state %q - creating a new order.", order.Status)
		return a.createOrder(ctx, cl, crt)
	case acme.StatusValid, acme.StatusPending, acme.StatusProcessing:
		return order, nil
	}

	return nil, fmt.Errorf("order %q unknown status: %q", order.URL, order.Status)
}

func (a *Acme) acceptChallenge(ctx context.Context, cl client.Interface, auth *acme.Authorization, challenge *acme.Challenge) error {
	glog.Infof("Accepting challenge for domain %q", auth.Identifier.Value)
	var err error
	challenge, err = cl.AcceptChallenge(ctx, challenge)
	if err != nil {
		return err
	}

	glog.Infof("Waiting for authorization for domain %q", auth.Identifier.Value)
	authorization, err := cl.WaitAuthorization(ctx, auth.URL)
	if err != nil {
		return err
	}

	if authorization.Status != acme.StatusValid {
		return fmt.Errorf("expected acme domain authorization status for %q to be valid, but it is %q", authorization.Identifier.Value, authorization.Status)
	}

	glog.Infof("Successfully authorized domain %q", auth.Identifier.Value)

	return nil
}

// presentAuthorization will present the challenge required for the given
// authorization using the supplied certificate configuration.
// If ths authorization is already presented, it will return no error.
// If the self-check for the authorization has passed, it will return true.
// Otherwise it will return false.
func (a *Acme) presentAuthorization(ctx context.Context, cl client.Interface, crt *v1alpha1.Certificate, auth *acme.Authorization) (bool, *acme.Challenge, error) {
	glog.Infof("Presenting challenge for domain %q", auth.Identifier.Value)
	challenge, err := a.challengeForAuthorization(cl, crt, auth)
	if err != nil {
		// TODO: handle error properly
		return false, nil, err
	}
	domain := auth.Identifier.Value
	token := challenge.Token
	key, err := keyForChallenge(cl, challenge)
	if err != nil {
		return false, challenge, err
	}
	solver, err := a.solverFor(challenge.Type)
	if err != nil {
		// TODO: handle error properly
		return false, challenge, err
	}
	err = solver.Present(ctx, crt, domain, token, key)
	if err != nil {
		// TODO: handle error properly
		return false, challenge, err
	}
	glog.Infof("Performing check to ensure challenge has propagated")
	ok, err := solver.Check(domain, token, key)
	if err != nil {
		return false, challenge, err
	}
	return ok, challenge, nil
}

// cleanupAuthorization will clean up a given authorization.
// To do this, it first determines the challenge type to use for the
// authorization based on the Issuer and Certificate configuration.
// It then calls CleanUp on the appropriate Solver.
// CleanUp will clean up any remaining resources left over from attempting to
// solve the given challenge.
// If a valid challenge type is not configured, cleanupAuthorization will
// return an error.
func (a *Acme) cleanupAuthorization(ctx context.Context, cl client.Interface, crt *v1alpha1.Certificate, auth *acme.Authorization) error {
	glog.Infof("Cleaning up authorization for %q", auth.Identifier.Value)
	challenge, err := a.challengeForAuthorization(cl, crt, auth)
	if err != nil {
		return err
	}
	domain := auth.Identifier.Value
	token := challenge.Token
	key, err := keyForChallenge(cl, challenge)
	if err != nil {
		return err
	}

	solver, err := a.solverFor(challenge.Type)
	if err != nil {
		return err
	}

	return solver.CleanUp(ctx, crt, domain, token, key)
}

// keyForChallenge will return the key to use for solving a given acme
// challenge.
// Only http-01 and dns-01 challenges are supported.
// An error will be returned if obtaining the key fails, or the challenge type
// is unsupported.
func keyForChallenge(cl client.Interface, challenge *acme.Challenge) (string, error) {
	var err error
	switch challenge.Type {
	case "http-01":
		return cl.HTTP01ChallengeResponse(challenge.Token)
	case "dns-01":
		return cl.DNS01ChallengeRecord(challenge.Token)
	default:
		err = fmt.Errorf("unsupported challenge type %s", challenge.Type)
	}
	return "", err
}

// getAuthorizations will query the ACME server for the Authorization resources
// for the given list of authorization URLs using the given ACME client.
// It will return an error if obtaining any of the given authorizations fails.
func getAuthorizations(ctx context.Context, cl client.Interface, urls ...string) ([]*acme.Authorization, error) {
	var authzs []*acme.Authorization
	for _, url := range urls {
		a, err := cl.GetAuthorization(ctx, url)
		if err != nil {
			return nil, err
		}
		authzs = append(authzs, a)
	}
	return authzs, nil
}

// partitionAuthorizations will split a list of Authorizations into failed,
// pending and valid states.
func partitionAuthorizations(authzs ...*acme.Authorization) (failed, pending, valid []*acme.Authorization) {
	for _, a := range authzs {
		switch a.Status {
		case acme.StatusDeactivated, acme.StatusInvalid, acme.StatusRevoked, acme.StatusUnknown:
			failed = append(failed, a)
		case acme.StatusPending, acme.StatusProcessing:
			pending = append(pending, a)
		case acme.StatusValid:
			valid = append(valid, a)
		}
	}
	return failed, pending, valid
}

// pickChallengeType will select a challenge type to used based on the types
// offered by the ACME server (i.e. auth.Challenges), the options configured on
// the Certificate resource (cfg) and the providers configured on the
// corresponding Issuer resource. If there is no challenge type that can be
// used, it will return an error.
func (a *Acme) pickChallengeType(domain string, auth *acme.Authorization, cfg []v1alpha1.ACMECertificateDomainConfig) (string, error) {
	for _, d := range cfg {
		for _, dom := range d.Domains {
			if dom == domain {
				for _, challenge := range auth.Challenges {
					switch {
					case challenge.Type == "http-01" && d.HTTP01 != nil && a.issuer.GetSpec().ACME.HTTP01 != nil:
						return challenge.Type, nil
					case challenge.Type == "dns-01" && d.DNS01 != nil && a.issuer.GetSpec().ACME.DNS01 != nil:
						return challenge.Type, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("no configured and supported challenge type found")
}

func (a *Acme) challengeForAuthorization(cl client.Interface, crt *v1alpha1.Certificate, auth *acme.Authorization) (*acme.Challenge, error) {
	domain := auth.Identifier.Value
	glog.Infof("picking challenge type for domain %q", domain)
	challengeType, err := a.pickChallengeType(domain, auth, crt.Spec.ACME.Config)
	if err != nil {
		return nil, fmt.Errorf("error picking challenge type to use for domain '%s': %s", domain, err.Error())
	}

	for _, challenge := range auth.Challenges {
		if challenge.Type != challengeType {
			continue
		}
		glog.Infof("picked challenge type %q for domain %q", challenge.Type, domain)
		return challenge, nil
	}
	return nil, fmt.Errorf("challenge mechanism '%s' not allowed for domain", challengeType)
}
