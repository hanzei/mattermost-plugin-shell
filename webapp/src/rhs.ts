// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

// The RHS close action is only available where the component is registered
// (index.tsx). This singleton bridges it to the terminal component so it can
// close the panel when the shell session ends.

type CloseFn = () => void;

let closeFn: CloseFn = () => {};

export function setCloseRHS(fn: CloseFn): void {
    closeFn = fn;
}

export function closeRHS(): void {
    closeFn();
}
