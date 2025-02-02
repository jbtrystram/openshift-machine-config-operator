package e2e_layering

import (
	"context"
	"flag"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/openshift/machine-config-operator/pkg/controller/build"
	"github.com/openshift/machine-config-operator/test/framework"
	"github.com/openshift/machine-config-operator/test/helpers"
	"github.com/stretchr/testify/require"

	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// The MachineConfigPool to create for the tests.
	layeredMCPName string = "layered"

	// The ImageStream name to use for the tests.
	imagestreamName string = "os-image"

	// The name of the global pull secret copy to use for the tests.
	globalPullSecretCloneName string = "global-pull-secret-copy"

	// The custom Dockerfile content to build for the tests.
	cowsayDockerfile string = `FROM quay.io/centos/centos:stream9 AS centos
RUN dnf install -y epel-release
FROM configs AS final
COPY --from=centos /etc/yum.repos.d /etc/yum.repos.d
COPY --from=centos /etc/pki/rpm-gpg/RPM-GPG-KEY-* /etc/pki/rpm-gpg/
RUN sed -i 's/\$stream/9-stream/g' /etc/yum.repos.d/centos*.repo && \
    rpm-ostree install cowsay`
)

var skipCleanup bool

func init() {
	// Skips running the cleanup functions. Useful for debugging tests.
	flag.BoolVar(&skipCleanup, "skip-cleanup", false, "Skips running the cleanup functions")
}

// Holds elements common for each on-cluster build tests.
type onClusterBuildTestOpts struct {
	// Which image builder type to use for the test.
	imageBuilderType string

	// The custom Dockerfiles to use for the test. This is a map of MachineConfigPool name to Dockerfile content.
	customDockerfiles map[string]string

	// What node(s) should be targeted for the test.
	targetNodes []*corev1.Node

	// What MachineConfigPool name to use for the test.
	poolName string
}

// Tests that an on-cluster build can be performed with the OpenShift Image Builder.
func TestOnClusterBuildsOpenshiftImageBuilder(t *testing.T) {
	runOnClusterBuildTest(t, onClusterBuildTestOpts{
		imageBuilderType: build.OpenshiftImageBuilder,
		poolName:         layeredMCPName,
		customDockerfiles: map[string]string{
			layeredMCPName: cowsayDockerfile,
		},
	})
}

// Tests tha an on-cluster build can be performed with the Custom Pod Builder.
func TestOnClusterBuildsCustomPodBuilder(t *testing.T) {
	runOnClusterBuildTest(t, onClusterBuildTestOpts{
		imageBuilderType: build.CustomPodImageBuilder,
		poolName:         layeredMCPName,
		customDockerfiles: map[string]string{
			layeredMCPName: cowsayDockerfile,
		},
	})
}

// Tests that an on-cluster build can be performed and that the resulting image
// is rolled out to an opted-in node.
func TestOnClusterBuildRollsOutImage(t *testing.T) {
	imagePullspec := runOnClusterBuildTest(t, onClusterBuildTestOpts{
		imageBuilderType: build.OpenshiftImageBuilder,
		poolName:         layeredMCPName,
		customDockerfiles: map[string]string{
			layeredMCPName: cowsayDockerfile,
		},
	})

	cs := framework.NewClientSet("")
	node := helpers.GetRandomNode(t, cs, "worker")
	t.Cleanup(makeIdempotentAndRegister(t, func() {
		helpers.DeleteNodeAndMachine(t, node)
	}))
	helpers.LabelNode(t, cs, node, helpers.MCPNameToRole(layeredMCPName))
	helpers.WaitForNodeImageChange(t, cs, node, imagePullspec)

	t.Log(helpers.ExecCmdOnNode(t, cs, node, "chroot", "/rootfs", "cowsay", "Moo!"))
}

// Sets up and performs an on-cluster build for a given set of parameters.
// Returns the built image pullspec for later consumption.
func runOnClusterBuildTest(t *testing.T, testOpts onClusterBuildTestOpts) string {
	cs := framework.NewClientSet("")

	t.Logf("Running with ImageBuilder type: %s", testOpts.imageBuilderType)

	prepareForTest(t, cs, testOpts)

	optPoolIntoLayering(t, cs, testOpts.poolName)

	t.Logf("Wait for build to start")
	waitForPoolToReachState(t, cs, testOpts.poolName, func(mcp *mcfgv1.MachineConfigPool) bool {
		return ctrlcommon.NewLayeredPoolState(mcp).IsBuilding()
	})

	t.Logf("Build started! Waiting for completion...")
	imagePullspec := ""
	waitForPoolToReachState(t, cs, testOpts.poolName, func(mcp *mcfgv1.MachineConfigPool) bool {
		lps := ctrlcommon.NewLayeredPoolState(mcp)
		if lps.HasOSImage() && lps.IsBuildSuccess() {
			imagePullspec = lps.GetOSImage()
			return true
		}

		if lps.IsBuildFailure() {
			t.Fatalf("Build unexpectedly failed.")
		}

		return false
	})

	t.Logf("MachineConfigPool %q has finished building. Got image: %s", testOpts.poolName, imagePullspec)

	return imagePullspec
}

// Adds the layeringEnabled label to the target MachineConfigPool and registers
// / returns a function to unlabel it.
func optPoolIntoLayering(t *testing.T, cs *framework.ClientSet, pool string) func() {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mcp, err := cs.MachineconfigurationV1Interface.MachineConfigPools().Get(context.TODO(), pool, metav1.GetOptions{})
		require.NoError(t, err)

		if mcp.Labels == nil {
			mcp.Labels = map[string]string{}
		}

		mcp.Labels[ctrlcommon.LayeringEnabledPoolLabel] = ""

		_, err = cs.MachineconfigurationV1Interface.MachineConfigPools().Update(context.TODO(), mcp, metav1.UpdateOptions{})
		if err == nil {
			t.Logf("Added label %q to MachineConfigPool %s to opt into layering", ctrlcommon.LayeringEnabledPoolLabel, pool)
		}
		return err
	})

	require.NoError(t, err)

	return makeIdempotentAndRegister(t, func() {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			mcp, err := cs.MachineconfigurationV1Interface.MachineConfigPools().Get(context.TODO(), pool, metav1.GetOptions{})
			require.NoError(t, err)

			delete(mcp.Labels, ctrlcommon.LayeringEnabledPoolLabel)

			_, err = cs.MachineconfigurationV1Interface.MachineConfigPools().Update(context.TODO(), mcp, metav1.UpdateOptions{})
			if err == nil {
				t.Logf("Removed label %q to MachineConfigPool %s to opt out of layering", ctrlcommon.LayeringEnabledPoolLabel, pool)
			}
			return err
		})

		require.NoError(t, err)
	})
}

// Prepares for an on-cluster build test by performing the following:
// - Gets the Docker Builder secret name from the MCO namespace.
// - Creates the imagestream to use for the test.
// - Clones the global pull secret into the MCO namespace.
// - Creates the on-cluster-build-config ConfigMap.
// - Creates the target MachineConfigPool and waits for it to get a rendered config.
// - Creates the on-cluster-build-custom-dockerfile ConfigMap.
//
// Each of the object creation steps registers an idempotent cleanup function
// that will delete the object at the end of the test.
func prepareForTest(t *testing.T, cs *framework.ClientSet, testOpts onClusterBuildTestOpts) {
	pushSecretName, err := getBuilderPushSecretName(cs)
	require.NoError(t, err)

	imagestreamName := "os-image"
	t.Cleanup(createImagestream(t, cs, imagestreamName))

	t.Cleanup(copyGlobalPullSecret(t, cs))

	finalPullspec, err := getImagestreamPullspec(cs, imagestreamName)
	require.NoError(t, err)

	cmCleanup := createConfigMap(t, cs, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      build.OnClusterBuildConfigMapName,
			Namespace: ctrlcommon.MCONamespace,
		},
		Data: map[string]string{
			build.BaseImagePullSecretNameConfigKey:  globalPullSecretCloneName,
			build.FinalImagePushSecretNameConfigKey: pushSecretName,
			build.FinalImagePullspecConfigKey:       finalPullspec,
			build.ImageBuilderTypeConfigMapKey:      testOpts.imageBuilderType,
		},
	})

	t.Cleanup(cmCleanup)

	t.Cleanup(makeIdempotentAndRegister(t, helpers.CreateMCP(t, cs, testOpts.poolName)))

	t.Cleanup(createCustomDockerfileConfigMap(t, cs, testOpts.customDockerfiles))

	_, err = helpers.WaitForRenderedConfig(t, cs, testOpts.poolName, "00-worker")
	require.NoError(t, err)
}
