// Copyright 2021 Canonical Ltd.

package cmd_test

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/juju/cmd/cmdtesting"
	gc "gopkg.in/check.v1"
	"sigs.k8s.io/yaml"

	apiparams "github.com/CanonicalLtd/jimm/api/params"
	"github.com/CanonicalLtd/jimm/cmd/jimmctl/cmd"
)

type addControllerSuite struct {
	jimmSuite
}

var _ = gc.Suite(&addControllerSuite{})

func (s *addControllerSuite) TestAddControllerSuperuser(c *gc.C) {
	info := s.APIInfo(c)
	params := apiparams.AddControllerRequest{
		Name:          "controller-1",
		CACertificate: info.CACert,
		Username:      info.Tag.Id(),
		Password:      info.Password,
		APIAddresses:  info.Addrs,
	}
	tmpdir, tmpfile := writeYAMLTempFile(c, params)
	defer os.RemoveAll(tmpdir)

	// alice is superuser
	bClient := s.userBakeryClient("alice")
	context, err := cmdtesting.RunCommand(c, cmd.NewAddControllerCommandForTesting(s.ClientStore, bClient), tmpfile)
	c.Assert(err, gc.IsNil)
	c.Assert(cmdtesting.Stdout(context), gc.Matches, `name: controller-1
uuid: deadbeef-1bad-500d-9000-4b1d0d06f00d
publicaddress: \"\"
apiaddresses:
- localhost:.*
cacertificate: |
  -----BEGIN CERTIFICATE-----
  MIID/jCCAmagAwIBAgIVANxsMrzsXrdpjjUoxWQm1RCkmWcqMA0GCSqGSIb3DQEB
  CwUAMCYxDTALBgNVBAoTBEp1anUxFTATBgNVBAMTDGp1anUgdGVzdGluZzAeFw0y
  MDA0MDgwNTI3NTBaFw0zMDA0MDgwNTMyNTBaMCYxDTALBgNVBAoTBEp1anUxFTAT
  BgNVBAMTDGp1anUgdGVzdGluZzCCAaIwDQYJKoZIhvcNAQEBBQADggGPADCCAYoC
  ggGBAOW4k2bmXXU3tJ8H5AsGkp8ENLJXzU4SCOCB+X0jPQRVpFtywBVD96z+l+qW
  ndGLIg5zMQTtZm71CaOw+8Sl03XU0f28Xrjf+FZCAPID1c7NBttUShbu84euFoCS
  C8yobj6JzLz7QswvkshYQ7JEZ88UXtVHqg6MGYFdu+cX/dE1jC7aHg9bus/P6bFH
  PVFcHVVxNbLy98Id1iB7i0s97H17nu9O7ZKMrAQAX6dfAELAFQVicdN3WpfwNXEj
  M2KIrqttpM8s6/57mi9UJFYGdAEDNkJr/dI506VdGLpiqTFhQK6ztfDfY08QbWk6
  iJn8vzWvNW8WthmBtEDpv+DL+a5SJSLSAIZn9sbuBBpiX+csZb66fYhKFFIUrIa5
  lrjw8yiHJ4kgsEZJSYaAn7guqmOv8clvy1E2JjsOfGycest6+1/mNdMRFgrMxdzD
  0M2yZ96zrdfF/QXpi7Hk7jFLzimuujNUpKFv7k+XObQFxeXnoFrYVkj3YT8hhYF0
  mGRkAwIDAQABoyMwITAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0TAQH/BAUwAwEB/zAN
  BgkqhkiG9w0BAQsFAAOCAYEAd7GrziPRmjoK3HDF10S+5NgoKYvkOuk2jDap2Qaq
  ZFjDvrDA2tr6U0FGY+Hz+QfvtgT+YpJB5IvABvSXdq37llwKGsiSOZSrpHyTsOB0
  VcZAF3GMk1nHYMr0o1xRV2gm/ax+vUEStj0k2gTs/p57uhKcCUXR0p3PWXKcRj9a
  nVf5bdVkt6ghGs7/uEvj303raUFSf5dJ4C9RTgBK2E9/wlBYNyj5vcsshNpz8kt6
  RuARM6Boq2EwKkpRlbvImDM8ZJJLwtpijYrx3egfOSEA7Wfwgwn+B359XcosOee5
  n5BC62EjaP85cM9HCtp2DwKjNSosxja723qZPY6Z0Y7IVn3JVjgC2kWP6GViwb+v
  l9uwx9ASHPz9ilh6gpjgIifOKZYCaBSS9g8VxHpO5Izxj4vi4AX5cebDg3SzDVik
  hZuWHpuOT120okoutwuUSU9448cXLGZfoCZjjdMKXmOj8EEec1diDP4mhegYGezD
  LQRNNlaY2ajLt0paowf/Xxb8
  -----END CERTIFICATE-----
cloudtag: cloud-dummy
cloudregion: dummy-region
username: admin
agentversion: .*
status:
  status: available
  info: ""
  data: .*
  since: null
`)
}

func (s *addControllerSuite) TestAddController(c *gc.C) {
	info := s.APIInfo(c)
	params := apiparams.AddControllerRequest{
		Name:          "controller-1",
		CACertificate: info.CACert,
		Username:      info.Tag.Id(),
		Password:      info.Password,
		APIAddresses:  info.Addrs,
	}
	tmpdir, tmpfile := writeYAMLTempFile(c, params)
	defer os.RemoveAll(tmpdir)

	// bob is not superuser
	bClient := s.userBakeryClient("bob")
	_, err := cmdtesting.RunCommand(c, cmd.NewAddControllerCommandForTesting(s.ClientStore, bClient), tmpfile)
	c.Assert(err, gc.ErrorMatches, `unauthorized \(unauthorized access\)`)
}

func writeYAMLTempFile(c *gc.C, payload interface{}) (string, string) {
	data, err := yaml.Marshal(payload)
	c.Assert(err, gc.Equals, nil)

	dir, err := ioutil.TempDir("", "add-controller-test")
	c.Assert(err, gc.Equals, nil)

	tmpfn := filepath.Join(dir, "tmp.yaml")
	err = ioutil.WriteFile(tmpfn, data, 0666)
	c.Assert(err, gc.Equals, nil)
	return dir, tmpfn
}