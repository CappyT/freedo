export interface Toast {
	id: number;
	kind: 'ok' | 'error';
	text: string;
}

class Toasts {
	list = $state<Toast[]>([]);
	#next = 1;

	push(kind: Toast['kind'], text: string) {
		const id = this.#next++;
		this.list = [...this.list, { id, kind, text }];
		setTimeout(() => this.dismiss(id), kind === 'error' ? 9000 : 4500);
	}

	dismiss(id: number) {
		this.list = this.list.filter((t) => t.id !== id);
	}
}

export const toasts = new Toasts();
