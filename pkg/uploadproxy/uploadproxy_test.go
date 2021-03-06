package uploadproxy

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"net/http"
	"net/http/httptest"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/token"
	"kubevirt.io/containerized-data-importer/pkg/util/cert"
	"kubevirt.io/containerized-data-importer/pkg/util/cert/fetcher"
	"kubevirt.io/containerized-data-importer/pkg/util/cert/triple"
)

type httpClientConfig struct {
	key    []byte
	cert   []byte
	caCert []byte
}

type validateSuccess struct{}

type validateFailure struct{}

func (*validateSuccess) Validate(string) (*token.Payload, error) {
	return &token.Payload{
		Operation: token.OperationUpload,
		Name:      "testpvc",
		Namespace: "default",
		Resource: metav1.GroupVersionResource{
			Group:    "",
			Version:  "v1",
			Resource: "persistentvolumeclaims",
		},
	}, nil
}

func (*validateFailure) Validate(string) (*token.Payload, error) {
	return nil, fmt.Errorf("Bad token")
}

func getPublicKeyEncoded() string {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).ToNot(HaveOccurred())

	publicKeyPem, err := cert.EncodePublicKeyPEM(&privateKey.PublicKey)
	Expect(err).ToNot(HaveOccurred())

	return string(publicKeyPem)
}

func getHTTPClientConfig() *httpClientConfig {
	caKeyPair, err := triple.NewCA("myca")
	Expect(err).ToNot(HaveOccurred())

	clientKeyPair, err := triple.NewClientKeyPair(caKeyPair, "testclient", []string{})
	Expect(err).ToNot(HaveOccurred())

	return &httpClientConfig{
		key:    cert.EncodePrivateKeyPEM(clientKeyPair.Key),
		cert:   cert.EncodeCertPEM(clientKeyPair.Cert),
		caCert: cert.EncodeCertPEM(caKeyPair.Cert),
	}
}

func newProxyRequest(authHeaderValue string) *http.Request {
	req, err := http.NewRequest("POST", common.UploadPathSync, strings.NewReader("data"))
	Expect(err).ToNot(HaveOccurred())

	if authHeaderValue != "" {
		req.Header.Set("Authorization", authHeaderValue)
	}
	return req
}

func newProxyHeadRequest(authHeaderValue string) *http.Request {
	req, err := http.NewRequest("HEAD", common.UploadPathSync, nil)
	Expect(err).ToNot(HaveOccurred())

	if authHeaderValue != "" {
		req.Header.Set("Authorization", authHeaderValue)
	}
	return req
}

func submitRequestAndCheckStatus(request *http.Request, expectedCode int, app *uploadProxyApp) {
	rr := httptest.NewRecorder()
	if app == nil {
		app = createApp()
	}

	app.ServeHTTP(rr, request)
	Expect(rr.Code).To(Equal(expectedCode))
}

func createApp() *uploadProxyApp {
	app := &uploadProxyApp{}
	app.initHandlers()
	return app
}

var _ = Describe("Certificate functions", func() {
	It("Get signing key", func() {
		publicKeyPEM := getPublicKeyEncoded()
		app := createApp()

		err := app.getSigningKey(publicKeyPEM)
		Expect(err).ToNot(HaveOccurred())
		Expect(app.tokenValidator).ToNot(BeNil())
	})

	It("Get upload server client", func() {
		certs := getHTTPClientConfig()
		certFetcher := &fetcher.MemCertFetcher{Cert: certs.cert, Key: certs.key}
		bundleFetcher := &fetcher.MemCertBundleFetcher{Bundle: certs.caCert}

		cc := &clientCreator{certFetcher: certFetcher, bundleFetcher: bundleFetcher}
		_, err := cc.CreateClient()
		Expect(err).ToNot(HaveOccurred())
	})
})

type fakeClientCreator struct {
	client *http.Client
}

func (fcc *fakeClientCreator) CreateClient() (*http.Client, error) {
	return fcc.client, nil
}

func setupProxyTests(handler http.HandlerFunc) *uploadProxyApp {
	server := httptest.NewServer(handler)

	urlResolver := func(string, string, string) string {
		return server.URL
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testpvc",
			Namespace: "default",
			Annotations: map[string]string{
				"cdi.kubevirt.io/storage.pod.phase": "Running",
				"cdi.kubevirt.io/storage.pod.ready": "true",
			},
		},
	}

	objects := []runtime.Object{}
	objects = append(objects, pvc)
	app := createApp()
	app.client = k8sfake.NewSimpleClientset(objects...)
	app.tokenValidator = &validateSuccess{}
	app.urlResolver = urlResolver
	app.clientCreator = &fakeClientCreator{client: server.Client()}

	return app
}

var _ = Describe("submit request and check status", func() {
	table.DescribeTable("Test proxy status code", func(statusCode int) {
		app := setupProxyTests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(statusCode)
		}))

		req := newProxyRequest("Bearer valid")
		submitRequestAndCheckStatus(req, statusCode, app)
	},
		table.Entry("Test OK", http.StatusOK),
		table.Entry("Test error", http.StatusInternalServerError),
	)
	table.DescribeTable("Test head proxy status code", func(statusCode int) {
		app := setupProxyTests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(statusCode)
		}))

		req := newProxyHeadRequest("Bearer valid")
		submitRequestAndCheckStatus(req, statusCode, app)
	},
		table.Entry("Test OK", http.StatusOK),
		table.Entry("Test error", http.StatusInternalServerError),
	)
	It("Invalid token", func() {
		app := createApp()
		app.tokenValidator = &validateFailure{}

		req := newProxyRequest("Bearer valid")

		submitRequestAndCheckStatus(req, http.StatusUnauthorized, app)
	})
	table.DescribeTable("Test proxy auth header", func(headerValue string, statusCode int) {
		req := newProxyRequest(headerValue)
		submitRequestAndCheckStatus(req, statusCode, nil)
	},
		table.Entry("No auth header", "", http.StatusBadRequest),
		table.Entry("Malformed auth header: invalid prefix", "Beereer valid", http.StatusBadRequest),
	)
	It("Test healthz", func() {
		req, err := http.NewRequest("GET", healthzPath, nil)
		Expect(err).ToNot(HaveOccurred())
		submitRequestAndCheckStatus(req, http.StatusOK, nil)
	})
})
