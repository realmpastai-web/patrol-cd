import { fireEvent } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { takeTestFindings } from './guards';

describe('test guards', () => {
  it('records console.error output', () => {
    console.error('a finding');
    expect(takeTestFindings()).toEqual([{ kind: 'console.error', detail: 'a finding' }]);
  });

  function submitAForm(): void {
    const form = document.createElement('form');
    const button = document.createElement('button');
    form.append(button);
    document.body.append(form);
    fireEvent.click(button);
    form.remove();
  }

  // These reach the jsdom instance's virtual console and never go through console.error, so a major
  // of vitest that binds that console differently loses them without any test failing.
  it.each([
    ['window.scrollTo', () => window.scrollTo(0, 0)],
    ['navigation', () => window.location.assign('https://example.test/')],
    ['window.open', () => void window.open('https://example.test/')],
    ['HTMLFormElement.prototype.requestSubmit', submitAForm],
  ])('records what jsdom does not implement: %s', (feature, act) => {
    act();
    expect(takeTestFindings()).toEqual([
      { kind: 'jsdom error', detail: expect.stringContaining(`Not implemented: ${feature}`) as string },
    ]);
  });
});
