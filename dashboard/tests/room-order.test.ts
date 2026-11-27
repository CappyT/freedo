import { describe, expect, it } from 'vitest';
import { orderRooms } from '../src/lib/room-order.ts';

const room = (number: number) => ({ zone: { number } });

describe('room tile order', () => {
	it('uses saved ranks and appends new rooms in their normal order', () => {
		expect(orderRooms([room(3), room(8), room(11), room(14)], [8, 3])).toEqual([
			room(8),
			room(3),
			room(11),
			room(14)
		]);
	});

	it('ignores deleted rooms and repeated stale ranks', () => {
		expect(orderRooms([room(3), room(8)], [99, 8, 8])).toEqual([room(8), room(3)]);
	});

	it('uses the first saved rank for repeated room numbers', () => {
		expect(orderRooms([room(3), room(8)], [8, 3, 8])).toEqual([room(8), room(3)]);
	});
});
