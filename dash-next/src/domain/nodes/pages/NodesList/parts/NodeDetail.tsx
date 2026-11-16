import { BadgeGroup, Button, Progress } from '@ctrliq/quantic-react';

import type { Pod } from '~/api/k8s/k8sSchemas';
import { ResourcePanel } from '~/domain/resources/components/ResourcePanel';
import { statusForPod } from '~/domain/workloads/utils/selectors';
import { ToneBadge } from '~/shared/components/ToneBadge/ToneBadge';
import { toAge, toUsageTone } from '~/shared/utils/formatters';
import { nameOf, namespaceOf } from '~/shared/utils/selectors';

import { badgeColorsByTone, podListNotLoaded, tonesByStatus } from '../../../constants';
import type { NodeRow } from '../../../types/nodes';
import {
  toCordonLabel,
  toCpuDetail,
  toDetailSubtitle,
  toLabelText,
  toMemoryDetail,
  toPodsText,
  toPodTotal,
  toUptime,
} from '../../../utils/formatters';
import { shownPercentOf } from '../../../utils/selectors';
import { toConditionRows, toSystemFacts } from '../../../utils/transforms';
import styles from './NodeDetail.module.scss';

type Props = {
  row: NodeRow;
  pods: Pod[];
  readAt: number;
  canWrite: boolean;
  onHostShellRequested: ((nodeName: string) => void) | undefined;
  onCordonToggled: (row: NodeRow) => void;
  onClosed: () => void;
};

export function NodeDetail({
  row,
  pods,
  readAt,
  canWrite,
  onHostShellRequested,
  onCordonToggled,
  onClosed,
}: Props) {
  const cpuPercent = shownPercentOf(row, row.cpuPercent);
  const memoryPercent = shownPercentOf(row, row.memoryPercent);

  return (
    <ResourcePanel
      title={row.name}
      subtitle={toDetailSubtitle(row)}
      onClosed={onClosed}
      footer={
        canWrite ? (
          <>
            {onHostShellRequested ? (
              <Button
                size="xs"
                variant="outline"
                onClick={() => {
                  onHostShellRequested(row.name);
                }}
              >
                Host shell
              </Button>
            ) : null}
            <Button
              size="xs"
              variant={row.isCordoned ? 'primary' : 'outline'}
              onClick={() => {
                onCordonToggled(row);
              }}
            >
              {toCordonLabel(row.isCordoned)}
            </Button>
          </>
        ) : null
      }
    >
      <p className={styles.state}>
        <ToneBadge tone={tonesByStatus[row.readiness]}>{row.readiness}</ToneBadge>
        {row.isCordoned ? <ToneBadge tone="warning">Cordoned</ToneBadge> : null}
        {toUptime(toAge(row.createdAt, readAt))}
      </p>

      <section aria-label="Capacity">
        <h3 className={styles.heading}>Capacity</h3>
        <dl className={styles.facts}>
          <dt>CPU</dt>
          <dd>
            <Progress
              value={cpuPercent}
              color={badgeColorsByTone[toUsageTone(cpuPercent) ?? 'success']}
              aria-label="CPU usage"
            />
            {toCpuDetail(cpuPercent, row.cpuCount)}
          </dd>
          <dt>Memory</dt>
          <dd>
            <Progress
              value={memoryPercent}
              color={badgeColorsByTone[toUsageTone(memoryPercent) ?? 'success']}
              aria-label="Memory usage"
            />
            {toMemoryDetail(memoryPercent)}
          </dd>
          <dt>Pods</dt>
          <dd>{toPodsText(row)}</dd>
        </dl>
      </section>

      <section aria-label="System info">
        <h3 className={styles.heading}>System info</h3>
        <dl className={styles.facts}>
          {toSystemFacts(row).map(({ label, value }) => (
            <div key={label} className={styles.fact}>
              <dt>{label}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      </section>

      <section aria-label="Conditions">
        <h3 className={styles.heading}>Conditions</h3>
        <dl className={styles.facts}>
          {toConditionRows(row).map(({ type, text, tone }) => (
            <div key={type} className={styles.fact}>
              <dt>{type}</dt>
              <dd>
                <ToneBadge tone={tone}>{text}</ToneBadge>
              </dd>
            </div>
          ))}
        </dl>
      </section>

      <section aria-label="Labels and taints">
        <h3 className={styles.heading}>Labels &amp; taints</h3>
        <BadgeGroup>
          {Object.entries(row.labels).map((label) => (
            <ToneBadge key={label[0]} tone="neutral">
              {toLabelText(label)}
            </ToneBadge>
          ))}
          {row.taints.length === 0 ? (
            <ToneBadge tone="neutral">no taints</ToneBadge>
          ) : (
            row.taints.map((taint) => (
              <ToneBadge key={taint} tone="warning">
                {taint}
              </ToneBadge>
            ))
          )}
        </BadgeGroup>
      </section>

      <section aria-label="Pods on node">
        <h3 className={styles.heading}>
          Pods on node <small>{toPodTotal(pods.length || row.podCount)}</small>
        </h3>
        {pods.length === 0 ? (
          <p className={styles.note}>{podListNotLoaded}</p>
        ) : (
          <ul className={styles.pods}>
            {pods.map((pod) => (
              <li key={`${namespaceOf(pod)}/${nameOf(pod)}`} className={styles.pod}>
                {nameOf(pod)}
                <ToneBadge tone="neutral">{statusForPod(pod)}</ToneBadge>
              </li>
            ))}
          </ul>
        )}
      </section>
    </ResourcePanel>
  );
}
