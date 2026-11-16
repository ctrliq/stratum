export type EventRow = {
  type: string;
  reason: string;
  object: string;
  namespace: string;
  message: string;
  count: number;
  age: string;
  lastSeenAt: number;
};

export type EventGroup = Pick<EventRow, 'type' | 'reason' | 'count' | 'age' | 'lastSeenAt'> & {
  objectCount: number;
};

export type TypeFilter = 'All' | 'Warning' | 'Normal';
