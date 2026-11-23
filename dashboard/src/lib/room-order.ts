export interface NumberedRoom {
	zone: { number: number };
}

/** Applies saved room ranks and leaves unlisted rooms in their normal order. */
export function orderRooms<T extends NumberedRoom>(rooms: readonly T[], order: readonly number[]): T[] {
	const rank = new Map<number, number>();
	order.forEach((number, index) => {
		if (!rank.has(number)) rank.set(number, index);
	});

	return rooms
		.map((room, index) => ({ room, index, rank: rank.get(room.zone.number) }))
		.sort((a, b) => {
			if (a.rank === undefined) return b.rank === undefined ? a.index - b.index : 1;
			if (b.rank === undefined) return -1;
			return a.rank - b.rank;
		})
		.map(({ room }) => room);
}
