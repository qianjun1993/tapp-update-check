/*
 * Tencent is pleased to support the open source community by making TKEStack available.
 *
 * Copyright (C) 2012-2019 Tencent. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not use
 * this file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/Apache-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OF ANY KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations under the License.
 */

package main

import (
	"context"
	"flag"
	"time"

	clientset "tkestack.io/tapp/pkg/client/clientset/versioned"
	informers "tkestack.io/tapp/pkg/client/informers/externalversions"
	"tkestack.io/tapp/pkg/version/verflag"
	"tkestack.io/tappupdate/pkg/tappupdate"

	"github.com/spf13/pflag"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"
	"k8s.io/klog"
)

var (
	masterURL  string
	kubeconfig string
	// kubeAPIQPS is the QPS to use while talking with kubernetes apiserver.
	kubeAPIQPS float32
	// kubeAPIBurst is the burst to use while talking with kubernetes apiserver.
	kubeAPIBurst int
	// TApp sync worker number
	worker int

	isOldVersion bool

	namespace string
)

const (
	defaultWorkerNumber = 5
	defaultKubeAPIQPS   = 2000
	defaultKubeAPIBurst = 2500
)

func main() {
	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	cfg.QPS = kubeAPIQPS
	cfg.Burst = kubeAPIBurst

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	tappClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building example clientset: %s", err.Error())
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	tappInformerFactory := informers.NewSharedInformerFactory(tappClient, time.Second*30)

	controller := tappupdate.NewController(kubeClient, tappClient, kubeInformerFactory, tappInformerFactory, isOldVersion)
	run := func(ctx context.Context) {
		stop := ctx.Done()

		go kubeInformerFactory.Start(stop)
		go tappInformerFactory.Start(stop)
		if err = controller.Run(worker, stop); err != nil {
			klog.Fatalf("Error running controller: %s", err.Error())
		}
	}
	run(context.TODO())
}

func init() {
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	addFlags(pflag.CommandLine)
	pflag.Parse()

	logs.InitLogs()
	defer logs.FlushLogs()

	verflag.PrintAndExitIfRequested()
}

func addFlags(fs *pflag.FlagSet) {
	fs.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&masterURL, "master", "",
		"The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	fs.Float32Var(&kubeAPIQPS, "kube-api-qps", defaultKubeAPIQPS, "QPS to use while talking with kubernetes apiserver")
	fs.IntVar(&kubeAPIBurst, "kube-api-burst", defaultKubeAPIBurst, "Burst to use while talking with kubernetes apiserver")
	fs.IntVar(&worker, "worker", defaultWorkerNumber, "TApp sync worker number, default: 5")
	fs.BoolVar(&isOldVersion, "is-old-vresion", true, "")
}
