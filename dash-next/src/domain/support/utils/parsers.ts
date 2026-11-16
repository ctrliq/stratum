import { isRecord } from '~/shared/utils/predicates';

export const fromReceipt = (receipt: unknown): string =>
  isRecord(receipt) && typeof receipt.collectionID === 'string' ? receipt.collectionID : '';
