package registry

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	acr "github.com/chrismellard/docker-credential-acr-env/pkg/credhelper"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/google"
)

var ecrHostRE = regexp.MustCompile(`^(\d{12}\.dkr[.-]ecr(-fips)?\.[a-z0-9-]+\.(amazonaws\.com(\.cn)?|on\.aws|sc2s\.sgov\.gov|c2s\.ic\.gov)|public\.ecr\.aws|ecr-public\.aws\.com)$`)

// DefaultKeychain resolves credentials in this order; the first source
// that has credentials for the registry wins, otherwise access is
// anonymous:
//
//  1. Docker config ($DOCKER_CONFIG/config.json or ~/.docker/config.json),
//     including credential helpers and credential stores. This covers
//     Docker Hub, GHCR, Quay and any other registry.
//  2. Google Application Default Credentials / workload identity, for
//     gcr.io and pkg.dev hosts only.
//  3. The AWS default credential chain (environment, IRSA, instance
//     profile), for ECR hosts only.
//  4. Azure environment credentials / managed identity, for ACR hosts only.
//
// A source that fails is skipped; its error is logged without credentials.
func DefaultKeychain(logger *slog.Logger) authn.Keychain {
	return authn.NewMultiKeychain(
		lenient{name: "docker-config", inner: authn.DefaultKeychain, logger: logger},
		lenient{name: "google", inner: google.Keychain, logger: logger, match: isGoogleHost},
		lenient{
			name:   "aws-ecr",
			inner:  authn.NewKeychainFromHelper(ecr.NewECRHelper(ecr.WithLogger(io.Discard))),
			logger: logger,
			match:  ecrHostRE.MatchString,
		},
		lenient{
			name:   "azure-acr",
			inner:  authn.NewKeychainFromHelper(acr.NewACRCredentialsHelper()),
			logger: logger,
			match:  isACRHost,
		},
	)
}

// lenient turns keychain errors into anonymous access and restricts a
// keychain to matching registry hosts.
type lenient struct {
	name   string
	inner  authn.Keychain
	logger *slog.Logger
	match  func(host string) bool
}

func (l lenient) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return l.ResolveContext(context.Background(), target)
}

func (l lenient) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	host := target.RegistryStr()
	if l.match != nil && !l.match(host) {
		return authn.Anonymous, nil
	}
	auth, err := authn.Resolve(ctx, l.inner, target)
	if err != nil {
		l.logger.Debug("credential source failed, continuing without it",
			"source", l.name, "registry", host, "error", err.Error())
		return authn.Anonymous, nil
	}
	return auth, nil
}

func isGoogleHost(host string) bool {
	return host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, ".pkg.dev")
}

func isACRHost(host string) bool {
	for _, suffix := range []string{".azurecr.io", ".azurecr.cn", ".azurecr.us", ".azurecr.de"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
