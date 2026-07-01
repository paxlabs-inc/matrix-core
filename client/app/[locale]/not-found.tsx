import { Link } from '@/i18n/navigation'
import { getTranslations } from 'next-intl/server'
import { Button } from '@/components/ui/button'
import { MatrixLogo } from '@/components/matrix/matrix-logo'

export default async function NotFound() {
  const t = await getTranslations('notFound')

  return (
    <div className="bg-background flex min-h-screen flex-col items-center justify-center gap-6 p-8 text-center">
      <MatrixLogo size="lg" />

      <div className="flex flex-col items-center gap-3">
        <span className="text-primary font-mono text-7xl font-bold tabular-nums" aria-hidden="true">
          {t('code')}
        </span>
        <h1 className="text-foreground text-lg font-semibold">{t('title')}</h1>
        <p className="text-muted-foreground max-w-sm text-sm">{t('message')}</p>
      </div>

      <Button asChild>
        <Link href="/">{t('back')}</Link>
      </Button>
    </div>
  )
}
