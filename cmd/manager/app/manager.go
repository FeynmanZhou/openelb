package app

import (
	"flag"
	"fmt"
	networkv1alpha2 "github.com/openelb/openelb/api/v1alpha2"
	"github.com/openelb/openelb/cmd/manager/app/options"
	"github.com/openelb/openelb/pkg/constant"
	"github.com/openelb/openelb/pkg/controllers/bgp"
	"github.com/openelb/openelb/pkg/controllers/ipam"
	"github.com/openelb/openelb/pkg/controllers/lb"
	"github.com/openelb/openelb/pkg/leader-elector"
	"github.com/openelb/openelb/pkg/log"
	"github.com/openelb/openelb/pkg/manager"
	"github.com/openelb/openelb/pkg/speaker"
	bgpd "github.com/openelb/openelb/pkg/speaker/bgp"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/util/term"
	clientset "k8s.io/client-go/kubernetes"
	cliflag "k8s.io/component-base/cli/flag"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

func NewOpenELBManagerCommand() *cobra.Command {
	s := options.NewOpenELBManagerOptions()

	cmd := &cobra.Command{
		Use:  "openelb-manager",
		Long: `The openelb manager is a daemon that `,
		Run: func(cmd *cobra.Command, args []string) {
			if errs := s.Validate(); len(errs) != 0 {
				fmt.Fprintf(os.Stderr, "%v\n", utilerrors.NewAggregate(errs))
				os.Exit(1)
			}

			if err := Run(s); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
		},
	}

	fs := cmd.Flags()

	namedFlagSets := s.Flags()
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	fs.AddFlagSet(pflag.CommandLine)

	usageFmt := "Usage:\n  %s\n"
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cmd.SetUsageFunc(func(cmd *cobra.Command) error {
		fmt.Fprintf(cmd.OutOrStderr(), usageFmt, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStderr(), namedFlagSets, cols)
		return nil
	})
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n"+usageFmt, cmd.Long, cmd.UseLine())
		cliflag.PrintSections(cmd.OutOrStdout(), namedFlagSets, cols)
	})

	return cmd
}

func Run(c *options.OpenELBManagerOptions) error {
	log.InitLog(c.LogOptions)

	setupLog := ctrl.Log.WithName("setup")

	mgr, err := manager.NewManager(ctrl.GetConfigOrDie(), c.GenericOptions)
	if err != nil {
		setupLog.Error(err, "unable to new manager")
		return err
	}

	bgpServer := bgpd.NewGoBgpd(c.Bgp)

	// Setup all Controllers
	err = ipam.SetupIPAM(mgr)
	if err != nil {
		setupLog.Error(err, "unable to setup ipam")
		return err
	}
	networkv1alpha2.Eip{}.SetupWebhookWithManager(mgr)

	err = bgp.SetupBgpConfReconciler(bgpServer, mgr)
	if err != nil {
		setupLog.Error(err, "unable to setup bgpconf")
	}

	err = bgp.SetupBgpPeerReconciler(bgpServer, mgr)
	if err != nil {
		setupLog.Error(err, "unable to setup bgppeer")
	}

	if err = lb.SetupServiceReconciler(mgr); err != nil {
		setupLog.Error(err, "unable to setup lb controller")
		return err
	}

	stopCh := ctrl.SetupSignalHandler()

	//For layer2
	k8sClient := clientset.NewForConfigOrDie(ctrl.GetConfigOrDie())
	leader.LeaderElector(stopCh, k8sClient, *c.Leader)

	//For gobgp
	err = speaker.RegisterSpeaker(constant.OpenELBProtocolBGP, bgpServer)
	if err != nil {
		setupLog.Error(err, "unable to register bgp speaker")
		return err
	}

	//For CNI
	err = speaker.RegisterSpeaker(constant.OpenELBProtocolDummy, speaker.NewFake())
	if err != nil {
		setupLog.Error(err, "unable to register dummy speaker")
		return err
	}
	hookServer := mgr.GetWebhookServer()

	setupLog.Info("registering webhooks to the webhook server")

	hookServer.Register("/validate-network-kubesphere-io-v1alpha2-svc", &webhook.Admission{Handler: &lb.SvcAnnotator{Client: mgr.GetClient()}})
	if err := mgr.Start(stopCh); err != nil {
		setupLog.Error(err, "unable to run the manager")
		return err
	}

	return nil
}
