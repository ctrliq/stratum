import type { DaemonSet, Info, StorageClass } from '~/api/k8s/k8sSchemas';
import { isDefaultStorageClass } from '~/domain/storage/utils/predicates';
import { absentText } from '~/shared/utils/formatters';
import { nameOf } from '~/shared/utils/selectors';

import { ciliumName, stratumBuildMarker } from '../constants';

export const kubernetesVersionOf = ({ gitVersion }: Partial<Pick<Info, 'gitVersion'>>): string =>
  (gitVersion ?? '').replace(/^v/, '');

export const cniNameOf = (daemonSets: Pick<DaemonSet, 'metadata'>[]): string =>
  daemonSets.some((daemonSet) => /cilium/i.test(nameOf(daemonSet))) ? ciliumName : absentText;

export const defaultStorageClassNameOf = (
  storageClasses: Pick<StorageClass, 'metadata'>[],
): string => storageClasses.filter(isDefaultStorageClass).map(nameOf).at(0) ?? absentText;

const stated = (value: string | undefined): string | undefined =>
  value && value !== absentText ? value : undefined;

export const regionOf = (region: string | undefined, zones: string[]): string =>
  stated(region) ?? zones.find((zone) => stated(zone) !== undefined) ?? absentText;

export const stratumVersionOf = (recorded: string | undefined, kubeletVersions: string[]): string =>
  stated(recorded) ??
  kubeletVersions.find((version) => version.includes(stratumBuildMarker)) ??
  absentText;

export const averageOf = (values: number[]): number =>
  values.length === 0
    ? 0
    : Math.round(values.reduce((total, value) => total + value, 0) / values.length);
