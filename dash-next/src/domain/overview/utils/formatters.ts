import { absentText } from '~/shared/utils/formatters';

import type { RunningCount } from '../types';

const separator = ' · ';

export const toVersionText = (version: string): string => `v${version.replace(/^v+/, '')}`;

export const toHeaderLine = (caption: string, region: string, version: string): string =>
  [caption, region, ...(version ? [`Kubernetes ${toVersionText(version)}`] : [])].join(separator);

export const toAsOf = (readAt: number): string =>
  `as of ${new Date(readAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;

export const toRatio = (part: number, whole: number): string => `${part} / ${whole}`;

export const toCapacityCaption = (percent: number): string => `${percent}% of capacity`;

export const toShortRole = (role: string): string => (role === 'control-plane' ? 'control' : role);

export const toNodeLinkTitle = (name: string): string => `Open ${name} on the Nodes page`;

export const toRunningText = ({ running, total }: RunningCount): string => `${running}/${total}`;

export const toRunningSentence = (count: RunningCount): string => `${toRunningText(count)} running`;

export const toKubernetesText = (version: string): string =>
  version ? toVersionText(version) : absentText;
