import clsx from 'clsx'
import { useEffect, useState } from 'react'
import { revealInFileManagerKey, useTranslate } from '../../i18n/index.js'
import type { FilePreview } from '../../mygo.js'
import {
  openWorkspacePath,
  readWorkspaceFilePreview,
  revealWorkspaceInFinder,
  saveWorkspacePathAs,
} from '../../services/workspace/store.js'
import { copyText } from '../../utils/clipboard.js'
import { Modal } from '../base/Modal.js'
import { showToast } from '../base/toastState.js'
import { Markdown } from '../markdown/Markdown.js'
import styles from './FilePreviewModal.module.css'

const formatFileSize = (bytes: number): string => {
  if (bytes < 1024) {
    return `${bytes} B`
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

export const FilePreviewModal = ({
  filePath,
  onClose,
}: {
  filePath: string
  onClose: () => void
}) => {
  const t = useTranslate()
  const [preview, setPreview] = useState<FilePreview | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [viewMode, setViewMode] = useState<'rendered' | 'source'>('rendered')

  useEffect(() => {
    let active = true
    setLoading(true)
    setError(null)
    setViewMode('rendered')
    void readWorkspaceFilePreview(filePath)
      .then((res) => {
        if (!active) {
          return
        }
        setPreview(res)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (!active) {
          return
        }
        setError(err instanceof Error ? err.message : String(err))
        setLoading(false)
      })
    return () => {
      active = false
    }
  }, [filePath])

  const canToggleRendered = preview?.kind === 'html' || preview?.kind === 'markdown'
  const canCopyText =
    preview && preview.kind !== 'binary' && preview.kind !== 'image' && preview.content.length > 0

  const handleSaveAs = async () => {
    try {
      const saved = await saveWorkspacePathAs(filePath)
      if (saved) {
        showToast(t('toolSaveAsSuccess', { path: saved }))
      }
    } catch {
      showToast(t('toolSaveAsFailed'))
    }
  }

  const handleCopy = async () => {
    if (!preview?.content) {
      return
    }
    const ok = await copyText(preview.content)
    showToast(ok ? t('copied') : t('copyFailed'))
  }

  return (
    <Modal
      closable
      className={styles.modal}
      mainClassName={styles.main}
      title={
        <div className={styles.titleBar}>
          <span className={styles.fileName} title={filePath}>
            {preview?.name ?? filePath.split(/[\\/]/).at(-1) ?? filePath}
          </span>
          {preview ? (
            <span className={styles.meta}>
              {preview.kind.toUpperCase()} · {formatFileSize(preview.size)}
            </span>
          ) : null}
        </div>
      }
      onClose={onClose}
      footer={
        <div className={styles.footer}>
          <span className={styles.pathText} title={filePath}>
            {filePath}
          </span>
          <div className={styles.footerActions}>
            {canToggleRendered ? (
              <div className={styles.modeSwitch}>
                <button
                  type="button"
                  className={clsx(styles.modeBtn, viewMode === 'rendered' && styles.modeBtnActive)}
                  onClick={() => setViewMode('rendered')}
                >
                  {t('toolPreviewModeRendered')}
                </button>
                <button
                  type="button"
                  className={clsx(styles.modeBtn, viewMode === 'source' && styles.modeBtnActive)}
                  onClick={() => setViewMode('source')}
                >
                  {t('toolPreviewModeSource')}
                </button>
              </div>
            ) : null}
            {canCopyText ? (
              <button type="button" className={styles.actionBtn} onClick={() => void handleCopy()}>
                {t('copy')}
              </button>
            ) : null}
            <button type="button" className={styles.actionBtn} onClick={() => void handleSaveAs()}>
              {t('toolSaveAs')}
            </button>
            <button
              type="button"
              className={styles.actionBtn}
              onClick={() => {
                void openWorkspacePath(filePath)
              }}
            >
              {t('toolOpenFile')}
            </button>
            <button
              type="button"
              className={styles.actionBtn}
              onClick={() => {
                void revealWorkspaceInFinder(filePath)
              }}
            >
              {t(revealInFileManagerKey)}
            </button>
          </div>
        </div>
      }
    >
      {loading ? (
        <div className={styles.placeholder}>{t('toolPreviewLoading')}</div>
      ) : error || !preview ? (
        <div className={styles.placeholder}>{error || t('toolPreviewFailed')}</div>
      ) : (
        <div className={styles.contentWrap}>
          {preview.truncated && preview.kind !== 'image' ? (
            <div className={styles.banner}>{t('toolPreviewTruncated')}</div>
          ) : null}

          {preview.kind === 'html' && viewMode === 'rendered' ? (
            <iframe
              title={preview.name}
              className={styles.iframe}
              sandbox="allow-scripts"
              srcDoc={preview.content}
            />
          ) : preview.kind === 'markdown' && viewMode === 'rendered' ? (
            <div className={styles.markdownWrap}>
              <Markdown
                content={preview.content}
                loading={false}
                disableLink={false}
                cacheKey={`preview:${preview.path}:${preview.size}`}
              />
            </div>
          ) : preview.kind === 'image' ? (
            preview.content ? (
              <div className={styles.imageWrap}>
                <img src={preview.content} alt={preview.name} className={styles.image} />
              </div>
            ) : (
              <div className={styles.placeholder}>{t('toolPreviewImageTooLarge')}</div>
            )
          ) : preview.kind === 'binary' ? (
            <div className={styles.placeholder}>{t('toolPreviewBinary')}</div>
          ) : (
            <pre className={styles.codePre}>{preview.content}</pre>
          )}
        </div>
      )}
    </Modal>
  )
}
