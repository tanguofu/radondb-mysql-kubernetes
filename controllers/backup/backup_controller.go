/*
Copyright 2021 RadonDB.

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

package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	v1beta1 "github.com/radondb/radondb-mysql-kubernetes/api/v1beta1"
	"github.com/radondb/radondb-mysql-kubernetes/utils"
	"gopkg.in/yaml.v2"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// BackupReconciler reconciles a Backup object.
type BackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Owner    client.FieldOwner
}
type BackupResource struct {
	cronjobs     []*batchv1beta1.CronJob
	jobs         []*batchv1.Job
	mysqlCluster *v1beta1.MysqlCluster
}

//+kubebuilder:rbac:groups=mysql.radondb.com,resources=backups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mysql.radondb.com,resources=backups/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Backup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("controllers").WithName("backup")

	result := reconcile.Result{}
	backup := &v1beta1.Backup{}

	if err := r.Client.Get(ctx, req.NamespacedName, backup); err != nil {
		// NotFound cannot be fixed by requeuing so ignore it. During background
		// deletion, we receive delete events from backup's dependents after
		// backup is deleted.
		if err = client.IgnoreNotFound(err); err != nil {
			log.Error(err, "unable to fetch Backup")
		}
		return result, err
	}
	//set default value

	// if backup.Spec.ClusterName is empty, return error
	if backup.Spec.ClusterName == "" {
		return result, errors.New("backup.Spec.ClusterName is empty")
	}
	// get MySQLCluster object
	cluster := &v1beta1.MysqlCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Spec.ClusterName,
			Namespace: backup.Namespace,
		},
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		// NotFound cannot be fixed by requeuing so ignore it. During background
		// deletion, we receive delete events from backup's dependents after
		// backup is deleted.
		if err = client.IgnoreNotFound(err); err != nil {
			log.Error(err, "unable to fetch MysqlCluster")
		}
	}

	var err error
	// Keep a copy of cluster prior to any manipulations.
	before := backup.DeepCopy()

	patchClusterStatus := func() (reconcile.Result, error) {
		if !equality.Semantic.DeepEqual(before.Status, backup.Status) {
			if err := errors.WithStack(r.Client.Status().Patch(
				ctx, backup, client.MergeFrom(before), r.Owner)); err != nil {
				log.Error(err, "patching cluster status")
				return result, err
			}
			log.V(1).Info("patched cluster status")
		}
		return result, err
	}

	// create the Result that will be updated while reconciling any/all backup resources

	backupResources, err := r.getBackupResources(ctx, backup)
	if err != nil {
		// exit early if can't get and clean existing resources as needed to reconcile
		return result, errors.WithStack(err)
	}
	backupResources.mysqlCluster = cluster
	if err := r.reconcileManualBackup(ctx, backup, backupResources.jobs, backupResources.mysqlCluster); err != nil {
		log.Error(err, "unable to reconcile manual backup")
	}
	if err := r.reconcileCronBackup(ctx, backup, backupResources.cronjobs, backupResources.jobs, cluster); err != nil {
		log.Error(err, "unable to reconcile cron backup")
	}
	return patchClusterStatus()
}

// SetupWithManager sets up the controller with the Manager.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Backup{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1beta1.CronJob{}).
		Complete(r)
}

func (r *BackupReconciler) getBackupResources(ctx context.Context,
	backup *v1beta1.Backup) (*BackupResource, error) {
	// get the cluster
	backupResource := &BackupResource{}
	gvks := []schema.GroupVersionKind{{
		Group:   batchv1.SchemeGroupVersion.Group,
		Version: batchv1.SchemeGroupVersion.Version,
		Kind:    "JobList",
	}, {
		Group:   batchv1beta1.SchemeGroupVersion.Group,
		Version: batchv1beta1.SchemeGroupVersion.Version,
		Kind:    "CronJobList",
	},
	}
	selector := BackupSelector(backup.Spec.ClusterName)
	for _, gvk := range gvks {
		uList := &unstructured.UnstructuredList{}
		uList.SetGroupVersionKind(gvk)
		if err := r.Client.List(ctx, uList,
			client.InNamespace(backup.GetNamespace()),
			client.MatchingLabelsSelector{Selector: selector}); err != nil {
			return nil, errors.WithStack(err)
		}
		if len(uList.Items) == 0 {
			continue
		}
		if err := unstructuredToBackupResources(gvk.Kind, backupResource,
			uList); err != nil {
			return nil, errors.WithStack(err)
		}

	}
	return backupResource, nil
}

func unstructuredToBackupResources(kind string, backupResource *BackupResource,
	uList *unstructured.UnstructuredList) error {
	for _, u := range uList.Items {
		switch kind {
		case "JobList":
			job := &batchv1.Job{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, job); err != nil {
				return errors.WithStack(err)
			}
			backupResource.jobs = append(backupResource.jobs, job)
		case "CronJobList":
			cronjob := &batchv1beta1.CronJob{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, cronjob); err != nil {
				return errors.WithStack(err)
			}
			backupResource.cronjobs = append(backupResource.cronjobs, cronjob)
		}
	}
	return nil
}

func (r *BackupReconciler) reconcileManualBackup(ctx context.Context,
	backup *v1beta1.Backup, manualBackupJobs []*batchv1.Job, cluster *v1beta1.MysqlCluster) error {

	log := log.FromContext(ctx).WithValues("reconcileManualBackup", "CronJob")
	manualStatus := backup.Status.ManualBackup
	var currentBackupJob *batchv1.Job
	if len(backup.ObjectMeta.Labels["cluster"]) == 0 {
		backup.ObjectMeta.Labels = labels.Set{"cluster": backup.Spec.ClusterName}
		if err := r.Update(ctx, backup); err != nil {
			return err
		}
	}
	if backup.Spec.BackupSchedule != nil {
		// if the backup is a scheduled backup, ignore manual backups
		return nil
	}

	// remove last if field more 5
	schedules := strings.Fields(backup.Spec.BackupSchedule.CronExpression)
	if len(schedules) > 5 {
		backup.Spec.BackupSchedule.CronExpression = strings.Join(schedules[:5], " ")
		log.Info("rewrite CronExpression", "orign", strings.Join(schedules, " "), "new", backup.Spec.BackupSchedule.CronExpression)
	}

	if len(manualBackupJobs) > 0 {
		for _, job := range manualBackupJobs {
			if job.GetOwnerReferences()[0].Name == backup.GetName() {
				currentBackupJob = job
				break
			}
		}

		if manualStatus != nil && currentBackupJob != nil {
			completed := jobCompleted(currentBackupJob)
			failed := jobFailed(currentBackupJob)
			manualStatus.CompletionTime = currentBackupJob.Status.CompletionTime
			manualStatus.StartTime = currentBackupJob.Status.StartTime
			manualStatus.Failed = currentBackupJob.Status.Failed
			manualStatus.Succeeded = currentBackupJob.Status.Succeeded
			manualStatus.Active = currentBackupJob.Status.Active
			if completed {
				manualStatus.BackupName = currentBackupJob.GetAnnotations()["backupName"]
				manualStatus.BackupSize = currentBackupJob.GetAnnotations()["backupSize"]
				manualStatus.BackupType = currentBackupJob.GetAnnotations()["backupType"]

			}
			if completed || failed {
				manualStatus.Finished = true
			}
			// Get State to the Status
			switch {
			case currentBackupJob.Status.Succeeded > 0:
				manualStatus.State = v1beta1.BackupSucceeded
			case currentBackupJob.Status.Active > 0:
				manualStatus.State = v1beta1.BackupActive
			case currentBackupJob.Status.Failed > 0:
				manualStatus.State = v1beta1.BackupFailed
			default:
				manualStatus.State = v1beta1.BackupStart
			}
			// return manual backup status to the backup status
			backup.Status.BackupName = manualStatus.BackupName
			backup.Status.BackupSize = manualStatus.BackupSize
			backup.Status.BackupType = manualStatus.BackupType
			backup.Status.State = manualStatus.State
			backup.Status.CompletionTime = manualStatus.CompletionTime
			backup.Status.StartTime = manualStatus.StartTime
			backup.Status.Type = v1beta1.ManualBackupInitiator

		}

	}

	// if there is an existing status, see if a new backup id has been provided, and if so reset
	// the status and proceed with reconciling a new backup
	if manualStatus == nil {
		manualStatus = &v1beta1.ManualBackupStatus{
			Finished: false,
		}
		backup.Status.ManualBackup = manualStatus
	}

	// if the status shows the Job is no longer in progress, then simply exit (which means a Job
	// that has reached a "completed" or "failed" status is no longer reconciled)
	if manualStatus != nil && manualStatus.Finished {
		return nil
	}

	backupJob := &batchv1.Job{}
	backupJob.ObjectMeta = ManualBackupJobMeta(cluster)
	if currentBackupJob != nil {
		backupJob.ObjectMeta.Name = currentBackupJob.ObjectMeta.Name
	}
	labels := ManualBackupLabels(cluster.Name)
	backupJob.ObjectMeta.Labels = labels

	spec, err := generateBackupJobSpec(backup, cluster, labels)
	if err != nil {
		return errors.WithStack(err)
	}

	backupJob.Spec = *spec

	backupJob.SetGroupVersionKind(batchv1.SchemeGroupVersion.WithKind("Job"))
	if err := controllerutil.SetControllerReference(backup, backupJob,
		r.Client.Scheme()); err != nil {
		return errors.WithStack(err)
	}

	if err := r.apply(ctx, backupJob); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (r *BackupReconciler) apply(ctx context.Context, object client.Object) error {

	log := log.FromContext(ctx).WithName("apply")

	yamlBytes, _ := yaml.Marshal(object)
	err := r.Client.Create(ctx, object)

	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			if err := r.Client.Update(ctx, object); err != nil {
				log.Error(err, "failed to update", "object", string(yamlBytes[:32]))
			}
		} else {
			log.Error(err, "failed to Create", "object", string(yamlBytes[:32]))
		}
	}

	return err
}

func (r *BackupReconciler) patch(
	ctx context.Context, object client.Object,
	patch client.Patch, options ...client.PatchOption,
) error {
	options = append([]client.PatchOption{r.Owner}, options...)
	err := r.Client.Patch(ctx, object, patch, options...)

	if err != nil {
		// yamlBytes, _ := yaml.Marshal(object)
		// patchBytes, _ := patch.Data(object)
		// log.Log.Error(err, fmt.Sprintf("BackupReconciler apply err: %v object:%s, patch:%s, options:%s", err, yamlBytes, patchBytes, options))
		log.Log.Error(err, fmt.Sprintf("BackupReconciler apply err: %v options: %v", err, options))
	}
	return err
}

func (r *BackupReconciler) reconcileCronBackup(ctx context.Context, backup *v1beta1.Backup,
	cronBackupJobs []*batchv1beta1.CronJob, BackupJobs []*batchv1.Job, cluster *v1beta1.MysqlCluster) error {
	log := log.FromContext(ctx).WithValues("backup", "CronJob")

	if backup.Spec.BackupSchedule == nil {
		// if the backup is a manual backup, ignore scheduled backups
		return nil
	}

	// remove last if field more 5
	schedules := strings.Fields(backup.Spec.BackupSchedule.CronExpression)
	if len(schedules) > 5 {
		backup.Spec.BackupSchedule.CronExpression = strings.Join(schedules[:5], " ")
		log.Info("rewrite eCronExpression", "orign", strings.Join(schedules, " "), "new", backup.Spec.BackupSchedule.CronExpression)
	}

	// Update backup.Status.ScheduledBackups
	scheduledStatus := []v1beta1.ScheduledBackupStatus{}
	for _, job := range BackupJobs {
		sbs := v1beta1.ScheduledBackupStatus{}
		if job.GetLabels()[LableCronJob] != "" {
			if len(job.GetOwnerReferences()) > 0 {
				sbs.CronJobName = job.OwnerReferences[0].Name
			}
			sbs.BackupName = job.GetAnnotations()["backupName"]
			sbs.BackupSize = job.GetAnnotations()["backupSize"]
			sbs.BackupType = job.GetAnnotations()["backupType"]
			sbs.CompletionTime = job.Status.CompletionTime
			sbs.Failed = job.Status.Failed
			sbs.Succeeded = job.Status.Succeeded
			sbs.StartTime = job.Status.StartTime
			if jobCompleted(job) || jobFailed(job) {
				sbs.Finished = true
			}
			switch {
			case job.Status.Succeeded > 0:
				sbs.State = v1beta1.BackupSucceeded
			case job.Status.Active > 0:
				sbs.State = v1beta1.BackupActive
			case job.Status.Failed > 0:
				sbs.State = v1beta1.BackupFailed
			default:
				sbs.State = v1beta1.BackupStart
			}
			scheduledStatus = append(scheduledStatus, sbs)
		}
	}
	// fill the backup status, always return the latest backup job status
	if len(scheduledStatus) > 0 {
		latestScheduledStatus := scheduledStatus[len(scheduledStatus)-1]
		backup.Status.StartTime = latestScheduledStatus.StartTime
		backup.Status.CompletionTime = latestScheduledStatus.CompletionTime
		backup.Status.BackupName = latestScheduledStatus.BackupName
		backup.Status.BackupSize = latestScheduledStatus.BackupSize
		backup.Status.Type = v1beta1.CronJobBackupInitiator
		backup.Status.State = latestScheduledStatus.State
		backup.Status.BackupType = latestScheduledStatus.BackupType
	}
	// file the scheduled backup status
	backup.Status.ScheduledBackups = scheduledStatus

	labels := CronBackupLabels(cluster.Name)
	objectMeta := CronBackupJobMeta(cluster)
	for _, cronjob := range cronBackupJobs {
		if cronjob.GetDeletionTimestamp() != nil {
			continue
		}
		if cronjob.GetLabels()[LabelCluster] == cluster.Name &&
			cronjob.GetLabels()[LableCronJob] == "true" {
			objectMeta = metav1.ObjectMeta{
				Namespace: backup.GetNamespace(),
				Name:      cronjob.Name,
			}

		}

	}
	objectMeta.Labels = labels
	// objectmeta.Annotations = annotations
	jobSpec, err := generateBackupJobSpec(backup, cluster, labels)
	if err != nil {
		return errors.WithStack(err)
	}
	suspend := (cluster.Status.State != v1beta1.ClusterReadyState) || (cluster.Spec.Standby != nil)
	cronJob := &batchv1beta1.CronJob{
		ObjectMeta: objectMeta,
		Spec: batchv1beta1.CronJobSpec{
			Schedule:          backup.Spec.BackupSchedule.CronExpression,
			Suspend:           &suspend,
			ConcurrencyPolicy: batchv1beta1.ForbidConcurrent,
			JobTemplate: batchv1beta1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: *jobSpec,
			},
		},
	}
	cronJob.SetGroupVersionKind(batchv1beta1.SchemeGroupVersion.WithKind("CronJob"))
	if err := controllerutil.SetControllerReference(backup, cronJob,
		r.Client.Scheme()); err != nil {
		return errors.WithStack(err)
	}

	if err := r.apply(ctx, cronJob); err != nil {
		// jsonBytes, _ := json.MarshalIndent(cronJob, "", "  ")
		log.Error(err, fmt.Sprintf("error: %v when attempting to create Backup CronJob", err))

	}

	return nil

}

func generateBackupJobSpec(backup *v1beta1.Backup, cluster *v1beta1.MysqlCluster, labels map[string]string) (*batchv1.JobSpec, error) {

	// If backup.Spec.BackupOpts.S3 is not nil then use ENV BACKUP_TYPE=s3 and set the s3SecretName
	// If backup.Spec.BackupOpts.NFS is not nil then use ENV BACKUP_TYPE=nfs and mount the nfs volume

	backupHost := GetBackupHost(cluster)
	backupImage := cluster.Spec.Backup.Image
	serviceAccountName := backup.Spec.ClusterName
	clusterAuthsctName := fmt.Sprintf("%s-secret", cluster.GetName())
	var S3BackuptEnv []corev1.EnvVar
	var NFSBackupEnv *corev1.EnvVar
	var backupTypeEnv corev1.EnvVar
	var NFSVolume *corev1.Volume
	var NFSVolumeMount *corev1.VolumeMount

	if backup.Spec.BackupOpts.S3 != nil && backup.Spec.BackupOpts.NFS != nil {
		return nil, errors.New("backup can only be configured with one of S3 or NFS")
	}

	if backup.Spec.BackupOpts.S3 != nil {
		s3SecretName := backup.Spec.BackupOpts.S3.BackupSecretName
		S3BackuptEnv = append(S3BackuptEnv,
			getEnvVarFromSecret(s3SecretName, "S3_ENDPOINT", "s3-endpoint", false),
			getEnvVarFromSecret(s3SecretName, "S3_ACCESSKEY", "s3-access-key", true),
			getEnvVarFromSecret(s3SecretName, "S3_SECRETKEY", "s3-secret-key", true),
			getEnvVarFromSecret(s3SecretName, "S3_BUCKET", "s3-bucket", true),
		)
		backupTypeEnv = corev1.EnvVar{Name: "BACKUP_TYPE", Value: "s3"}

	}

	if backup.Spec.BackupOpts.NFS != nil {
		NFSVolume = &corev1.Volume{
			Name:         "nfs-backup",
			VolumeSource: corev1.VolumeSource{NFS: &backup.Spec.BackupOpts.NFS.Volume},
		}
		NFSVolumeMount = &corev1.VolumeMount{
			Name:      "nfs-backup",
			MountPath: "/backup",
		}
		backupTypeEnv = corev1.EnvVar{Name: "BACKUP_TYPE", Value: "nfs"}

	}

	container := corev1.Container{
		Env: []corev1.EnvVar{
			{Name: "CONTAINER_TYPE", Value: utils.ContainerBackupJobName},
			{Name: "NAMESPACE", Value: cluster.Namespace},
			{Name: "CLUSTER_NAME", Value: cluster.GetName()},
			{Name: "SERVICE_NAME", Value: fmt.Sprintf("%s-mysql", cluster.GetName())},
			{Name: "HOST_NAME", Value: backupHost},
			{Name: "JOB_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.labels['job-name']",
				},
			}},
		},
		Image:           backupImage,
		ImagePullPolicy: cluster.Spec.ImagePullPolicy,
		Name:            utils.ContainerBackupName,
	}
	container.Args = []string{
		"request_a_backup",
		func() string {
			if len(backup.Spec.BackupOpts.BackupHost) != 0 {
				return GetBackupURL(cluster.Name, backup.Spec.BackupOpts.BackupHost, cluster.Namespace)
			} else {
				return GetXtrabackupURL(GetBackupHost(cluster))
			}
		}(),
	}
	// Add backup user and password to the env
	container.Env = append(container.Env,
		getEnvVarFromSecret(clusterAuthsctName, "BACKUP_USER", "backup-user", true),
		getEnvVarFromSecret(clusterAuthsctName, "BACKUP_PASSWORD", "backup-password", true),
	)
	if NFSBackupEnv != nil {
		container.Env = append(container.Env, *NFSBackupEnv)
	}
	if len(S3BackuptEnv) != 0 {
		container.Env = append(container.Env, S3BackuptEnv...)
	}

	if NFSVolumeMount != nil {
		container.VolumeMounts = append(container.VolumeMounts, *NFSVolumeMount)
	}

	container.Env = append(container.Env, backupTypeEnv)

	jobSpec := &batchv1.JobSpec{
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				Containers:         []corev1.Container{container},
				RestartPolicy:      corev1.RestartPolicyNever,
				ServiceAccountName: serviceAccountName,
				ImagePullSecrets:   cluster.Spec.ImagePullSecrets,
			},
		},
	}
	if NFSVolume != nil {
		jobSpec.Template.Spec.Volumes = []corev1.Volume{*NFSVolume}
	}
	var backoffLimit int32 = 1

	jobSpec.Template.Spec.Tolerations = cluster.Spec.Tolerations
	jobSpec.Template.Spec.Affinity = cluster.Spec.Affinity
	jobSpec.BackoffLimit = &backoffLimit
	return jobSpec, nil
}
