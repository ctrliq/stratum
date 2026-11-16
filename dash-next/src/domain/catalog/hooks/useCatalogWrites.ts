import {
  useCreateStratumCiqComV1NamespacedHelmChart,
  useDeleteStratumCiqComV1NamespacedHelmChart,
} from '~/api/k8s/k8sComponents';
import type { HelmChart } from '~/api/k8s/k8sSchemas';
import { toast } from '~/shared/components/Toaster/toast';
import { toErrorNotice } from '~/shared/utils/formatters';
import { labelOf } from '~/shared/utils/selectors';

import { appLabel, catalogNamespace } from '../constants';
import type { CatalogApp, InstallValues } from '../types/catalog';
import {
  toDroppedNotice,
  toInstallFailureNotice,
  toInstallingNotice,
  toUninstallingNotice,
  toUntargetedNotice,
} from '../utils/formatters';
import { hasDroppedValues } from '../utils/predicates';
import { droppedValuesOf, uninstallTargetOf } from '../utils/selectors';
import { toHelmChart } from '../utils/transforms';

export const useCatalogWrites = (onSettled: () => void) => {
  const creation = useCreateStratumCiqComV1NamespacedHelmChart();
  const deletion = useDeleteStratumCiqComV1NamespacedHelmChart();

  const onInstalled = (app: CatalogApp, values: InstallValues): void => {
    const chart = toHelmChart(app, values);
    creation.mutate(
      { pathParams: { namespace: catalogNamespace }, body: chart },
      {
        onSuccess: (created) => {
          const dropped = droppedValuesOf(chart, created);
          if (hasDroppedValues(dropped)) {
            toast.warning(toDroppedNotice(app, dropped));
          } else {
            toast.success(toInstallingNotice(app, values.targetNamespace));
          }
        },
        onError: (error) => {
          toast.error(toInstallFailureNotice(error, values.releaseName));
        },
        onSettled,
      },
    );
  };

  const onUninstalled = (app: CatalogApp, chart: HelmChart): void => {
    const target = uninstallTargetOf(chart);
    if (target.name && target.namespace) {
      deletion.mutate(
        { pathParams: { namespace: target.namespace, name: target.name } },
        {
          onSuccess: () => {
            toast.success(toUninstallingNotice(app, target));
          },
          onError: (error) => {
            toast.error(toErrorNotice(error));
          },
          onSettled,
        },
      );
    } else {
      toast.error(toUntargetedNotice(app));
    }
  };

  return {
    isBusy: creation.isPending || deletion.isPending,
    installingId:
      creation.isPending && creation.variables.body
        ? labelOf(creation.variables.body, appLabel)
        : undefined,
    onInstalled,
    onUninstalled,
  };
};
