"use client";

import * as React from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";

export function ReopenTaskDialog({
  taskId,
  trigger,
  onReopened,
}: {
  taskId: string;
  trigger: React.ReactNode;
  onReopened?: () => void;
}) {
  const [open, setOpen] = React.useState(false);
  const [reason, setReason] = React.useState("");
  const [pending, setPending] = React.useState(false);
  const trimmed = reason.trim();

  async function submit() {
    if (!trimmed || pending) return;
    setPending(true);
    try {
      const result = await api.reopenTask(taskId, trimmed);
      toast.success(result.queued ? `任务 #${taskId} 已打回，正在排队` : `任务 #${taskId} 已打回并继续`);
      setReason("");
      setOpen(false);
      onReopened?.();
    } catch (error) {
      toast.error(`打回失败：${(error as Error).message}`);
    } finally {
      setPending(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        setOpen(next);
        if (!next) setReason("");
      }}
    >
      <DialogTrigger asChild>{trigger}</DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>打回任务 #{taskId}</DialogTitle>
          <DialogDescription>
            图谱、漏洞、资产和会话都会保留。已达成的目标会重新打开，规划器会按你的说明继续。
          </DialogDescription>
        </DialogHeader>
        <Textarea
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="哪里没做对？例如：覆盖度只有 40%，目标要求 100%"
          aria-label="打回原因"
          rows={4}
          maxLength={4000}
          disabled={pending}
        />
        <DialogFooter>
          <Button variant="outline" disabled={pending} onClick={() => setOpen(false)}>
            取消
          </Button>
          <Button disabled={pending || !trimmed} onClick={() => void submit()}>
            {pending ? "提交中…" : "打回继续"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
