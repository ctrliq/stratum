import { describe, expect, it } from 'vitest';

import { toHelperJob, toPodOptions } from './transforms';

describe('toHelperJob', () => {
  const job = toHelperJob('worker-1', 'registry.local/busybox:1');
  const labels = { app: 'stratum-node-shell', 'stratum.ciq.com/node-shell': 'worker-1' };

  it('is a one-shot Job bounded by the cluster', () => {
    expect(job).toMatchObject({
      apiVersion: 'batch/v1',
      kind: 'Job',
      metadata: { generateName: 'stratum-node-shell-', labels },
      spec: { backoffLimit: 0, activeDeadlineSeconds: 3600, ttlSecondsAfterFinished: 600 },
    });
  });

  it('pins a privileged host-PID pod to the node with the server image', () => {
    expect(job.spec?.template).toEqual({
      metadata: { labels },
      spec: {
        nodeName: 'worker-1',
        hostPID: true,
        restartPolicy: 'Never',
        activeDeadlineSeconds: 3600,
        tolerations: [{ operator: 'Exists' }],
        containers: [
          {
            name: 'shell',
            image: 'registry.local/busybox:1',
            command: ['sleep', '3600'],
            stdin: true,
            tty: true,
            securityContext: { privileged: true },
          },
        ],
      },
    });
  });
});

describe('toPodOptions', () => {
  it('lists running pods as namespace / name', () => {
    expect(toPodOptions(['shop/web-1', 'test/api-0'], 'shop/web-1')).toEqual([
      ['shop / web-1', 'shop/web-1'],
      ['test / api-0', 'test/api-0'],
    ]);
  });

  it('says so when no pod is running', () => {
    expect(toPodOptions([], '')).toEqual([['no running pods', '']]);
  });

  it('keeps an unselected entry while no pod was picked', () => {
    expect(toPodOptions(['shop/web-1'], '')).toEqual([
      ['', ''],
      ['shop / web-1', 'shop/web-1'],
    ]);
  });
});
