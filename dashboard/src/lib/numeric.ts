/**
 * The value to keep after an event of a number field. While a person types "21.5", the field
 * passes through "21." and reads as NaN there. Storing that NaN would clear the field, and the
 * next key would start over. So an input event with an incomplete number keeps the stored value.
 * The change event comes when the person leaves the field: an empty or broken field is stored as
 * NaN then, so that validation names it.
 */
export function typedNumber(read: number, stored: number, leaving: boolean): number {
	return Number.isNaN(read) && !leaving ? stored : read;
}
