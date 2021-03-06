// Copyright 2017 The etcd-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backup

import (
	"crypto/tls"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/coreos/etcd-operator/pkg/backup/backend"
	"github.com/coreos/etcd-operator/pkg/backup/backupapi"
	"github.com/coreos/etcd-operator/pkg/backup/util"
	"github.com/coreos/etcd-operator/pkg/backup/writer"
	"github.com/coreos/etcd-operator/pkg/util/constants"
	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"github.com/coreos/etcd-operator/pkg/util/k8sutil"

	"github.com/coreos/etcd/clientv3"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// BackupManager backups an etcd cluster.
type BackupManager struct {
	kubecli kubernetes.Interface

	clusterName   string
	namespace     string
	etcdTLSConfig *tls.Config

	be backend.Backend
	bw writer.Writer
}

// NewBackupManager creates a BackupManager.
func NewBackupManager(kubecli kubernetes.Interface, clusterName string, namespace string, etcdTLSConfig *tls.Config, be backend.Backend) *BackupManager {
	return &BackupManager{
		kubecli:       kubecli,
		clusterName:   clusterName,
		namespace:     namespace,
		etcdTLSConfig: etcdTLSConfig,
		be:            be,
	}
}

// NewBackupManagerFromWriter creates a BackupManager with backup writer.
func NewBackupManagerFromWriter(kubecli kubernetes.Interface, bw writer.Writer, clusterName, namespace string) *BackupManager {
	return &BackupManager{
		kubecli:     kubecli,
		clusterName: clusterName,
		namespace:   namespace,
		bw:          bw,
	}
}

// SaveSnap saves the latest snapshot if its revision is greater than the given lastSnapRev
// and returns a BackupStatus containing saving backup metadata if SaveSnap succeeds.
func (bm *BackupManager) SaveSnap(lastSnapRev int64) (*backupapi.BackupStatus, error) {
	etcdcli, rev, err := bm.etcdClientWithMaxRevision()
	if err != nil {
		return nil, fmt.Errorf("create etcd client with max revision failed: %v", err)
	}
	defer etcdcli.Close()

	if rev <= lastSnapRev {
		logrus.Info("skipped creating new backup: no change since last time")
		return nil, nil
	}

	bs, err := bm.writeSnap(etcdcli.Maintenance, etcdcli.Endpoints()[0], rev)
	if err != nil {
		return nil, fmt.Errorf("write snapshot failed: %v", err)
	}
	logrus.Infof("saved backup (rev: %v, etcdVersion: %v) for cluster (%s)",
		bs.Revision, bs.Version, bm.clusterName)
	return bs, nil
}

func (bm *BackupManager) writeSnap(mcli clientv3.Maintenance, endpoint string, rev int64) (*backupapi.BackupStatus, error) {
	start := time.Now()

	version, err := getEtcdVersion(mcli, endpoint)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultSnapshotTimeout)
	rc, err := mcli.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to receive snapshot (%v)", err)
	}
	defer cancel()
	defer rc.Close()

	n, err := bm.be.Save(version, rev, rc)
	if err != nil {
		return nil, err
	}

	bs := &backupapi.BackupStatus{
		CreationTime:     time.Now().Format(time.RFC3339),
		Size:             util.ToMB(n),
		Version:          version,
		Revision:         rev,
		TimeTookInSecond: int(time.Since(start).Seconds() + 1),
	}

	return bs, nil
}

// SaveSnapWithPrefix uses backup writer to save latest snapshot to a path prepended with the given prefix
// and returns file size and full path.
// the full path has the format of prefix/<etcd_version>_<snapshot_reversion>_etcd.backup
// e.g prefix = etcd-backups/v1/default/example-etcd-cluster and
// backup object name = 3.1.8_0000000000000001_etcd.backup
// full path is "etcd-backups/v1/default/example-etcd-cluster/3.1.8_0000000000000001_etcd.backup".
func (bm *BackupManager) SaveSnapWithPrefix(prefix string) (string, error) {
	etcdcli, rev, err := bm.etcdClientWithMaxRevision()
	if err != nil {
		return "", fmt.Errorf("create etcd client failed: %v", err)
	}
	defer etcdcli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultSnapshotTimeout)
	rc, err := etcdcli.Snapshot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to receive snapshot (%v)", err)
	}
	defer cancel()
	defer rc.Close()

	version, err := getEtcdVersion(etcdcli.Maintenance, etcdcli.Endpoints()[0])
	if err != nil {
		return "", err
	}
	fullPath := path.Join(prefix, util.MakeBackupName(version, rev))
	_, err = bm.bw.Write(fullPath, rc)
	if err != nil {
		return "", fmt.Errorf("failed to write snapshot (%v)", err)
	}
	return fullPath, nil
}

func getEtcdVersion(mcli clientv3.Maintenance, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultSnapshotTimeout)
	resp, err := mcli.Status(ctx, endpoint)
	cancel()
	if err != nil {
		return "", fmt.Errorf("failed to receive etcd version (%v)", err)
	}
	return resp.Version, nil
}

// etcdClientWithMaxRevision gets the etcd member with the maximum kv store revision
// and returns the etcd client and the rev of that member.
func (bm *BackupManager) etcdClientWithMaxRevision() (*clientv3.Client, int64, error) {
	podList, err := bm.kubecli.Core().Pods(bm.namespace).List(k8sutil.ClusterListOpt(bm.clusterName))
	if err != nil {
		return nil, 0, err
	}

	var pods []*v1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase == v1.PodRunning {
			pods = append(pods, pod)
		}
	}

	if len(pods) == 0 {
		return nil, 0, errors.New("no running etcd pods found")
	}
	member, rev := getMemberWithMaxRev(pods, bm.etcdTLSConfig)
	if member == nil {
		return nil, 0, errors.New("no reachable member")
	}

	etcdcli, err := createEtcdClient(member.ClientURL(), bm.etcdTLSConfig)
	if err != nil {
		return nil, 0, fmt.Errorf("create etcd client failed: %v", err)
	}
	return etcdcli, rev, nil
}

func getMemberWithMaxRev(pods []*v1.Pod, tc *tls.Config) (*etcdutil.Member, int64) {
	var member *etcdutil.Member
	maxRev := int64(0)
	for _, pod := range pods {
		m := &etcdutil.Member{
			Name:         pod.Name,
			Namespace:    pod.Namespace,
			SecureClient: tc != nil,
		}
		cfg := clientv3.Config{
			Endpoints:   []string{m.ClientURL()},
			DialTimeout: constants.DefaultDialTimeout,
			TLS:         tc,
		}
		etcdcli, err := clientv3.New(cfg)
		if err != nil {
			logrus.Warningf("failed to create etcd client for pod (%v): %v", pod.Name, err)
			continue
		}
		defer etcdcli.Close()

		ctx, cancel := context.WithTimeout(context.Background(), constants.DefaultRequestTimeout)
		resp, err := etcdcli.Get(ctx, "/", clientv3.WithSerializable())
		cancel()
		if err != nil {
			logrus.Warningf("getMaxRev: failed to get revision from member %s (%s)", m.Name, m.ClientURL())
			continue
		}

		logrus.Infof("getMaxRev: member %s revision (%d)", m.Name, resp.Header.Revision)
		if resp.Header.Revision > maxRev {
			maxRev = resp.Header.Revision
			member = m
		}
	}
	return member, maxRev
}

func (b *BackupManager) getLatestBackupRev() int64 {
	// If there is any error, we just exit backup sidecar because we can't serve the backup any way.
	name, err := b.be.GetLatest()
	if err != nil {
		logrus.Fatal(err)
	}
	if len(name) == 0 {
		return 0
	}
	return util.MustParseRevision(name)
}

func createEtcdClient(url string, tlsConfig *tls.Config) (*clientv3.Client, error) {
	cfg := clientv3.Config{
		Endpoints:   []string{url},
		DialTimeout: constants.DefaultDialTimeout,
		TLS:         tlsConfig,
	}
	return clientv3.New(cfg)
}
