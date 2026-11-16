import { secretKeysColumn, secretTypeColumn } from '../constants';
import type { SecretListings, SecretTable } from '../types/config';
import { isSecretTable } from './predicates';

const cellUnder = (
  { columnDefinitions }: Pick<SecretTable, 'columnDefinitions'>,
  cells: unknown[],
  column: string,
): unknown => cells[columnDefinitions.findIndex(({ name }) => name === column)];

export const fromSecretTable = (answer: unknown): SecretListings => ({
  items: isSecretTable(answer)
    ? answer.rows.map(({ cells, object }) => {
        const type = cellUnder(answer, cells, secretTypeColumn);
        const keyCount = cellUnder(answer, cells, secretKeysColumn);
        return {
          metadata: object.metadata,
          type: typeof type === 'string' ? type : undefined,
          keyCount: typeof keyCount === 'number' ? keyCount : 0,
        };
      })
    : [],
});
