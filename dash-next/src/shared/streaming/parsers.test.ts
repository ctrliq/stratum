import { describe, expect, it } from 'vitest';

import { fromCommand, fromExecFrame, fromLogChunk } from './parsers';

const toFrame = (channel: number, text: string): ArrayBuffer =>
  Uint8Array.from([channel, ...new TextEncoder().encode(text)]).buffer;

const toText = (frame: ReturnType<typeof fromExecFrame>): string =>
  frame.type === 'output' ? new TextDecoder().decode(frame.bytes) : frame.type;

describe('fromCommand', () => {
  it('defaults to /bin/sh', () => {
    expect([fromCommand(undefined), fromCommand('  ')]).toEqual([['/bin/sh'], ['/bin/sh']]);
  });

  it('splits on spaces without interpreting quotes', () => {
    expect([fromCommand('bash  -c ls'), fromCommand('sh -c "echo hi"')]).toEqual([
      ['bash', '-c', 'ls'],
      ['sh', '-c', '"echo', 'hi"'],
    ]);
  });
});

describe('fromExecFrame', () => {
  it('reads stdout and stderr alike as output', () => {
    expect([
      toText(fromExecFrame(toFrame(1, 'héllo'))),
      toText(fromExecFrame(toFrame(2, 'oops'))),
    ]).toEqual(['héllo', 'oops']);
  });

  it('reads a Failure status with its message, or a stock one', () => {
    expect([
      fromExecFrame(toFrame(3, '{"status":"Failure","message":"container not found"}')),
      fromExecFrame(toFrame(3, '{"status":"Failure"}')),
    ]).toEqual([
      { type: 'failure', message: 'container not found' },
      { type: 'failure', message: 'exec error' },
    ]);
  });

  it('ignores other statuses, invalid JSON, empty frames and unknown channels', () => {
    expect(
      [
        toFrame(3, '{"status":"Success"}'),
        toFrame(3, 'not json'),
        new ArrayBuffer(0),
        toFrame(0, 'echo'),
        toFrame(4, '{}'),
        toFrame(9, 'x'),
      ].map((frame) => fromExecFrame(frame).type),
    ).toEqual(['ignored', 'ignored', 'ignored', 'ignored', 'ignored', 'ignored']);
  });
});

describe('fromLogChunk', () => {
  it('keeps an unfinished line for the next chunk', () => {
    expect(fromLogChunk('par', 'tial\nwhole\nnext')).toEqual({
      lines: ['partial', 'whole'],
      pending: 'next',
    });
  });

  it('holds everything when no newline arrived', () => {
    expect(fromLogChunk('a', 'b')).toEqual({ lines: [], pending: 'ab' });
  });
});
