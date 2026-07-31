'use client'

import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'
import { useTranslations } from 'next-intl'
import {
  FormField,
  TextAreaField,
  SelectField,
  useErrorHandler,
  validation,
  zodResolver,
} from '@/components/form-system'

// --- Schema ---

const contactSchema = z.object({
  name: validation.requiredString({ min: 2, max: 100 }),
  email: validation.email(),
  subject: validation.select(),
  message: validation.requiredString({ min: 10, max: 2000 }),
  priority: validation.select(),
})

type ContactFormValues = z.infer<typeof contactSchema>

// --- Component ---

export default function FormExamplePage() {
  const t = useTranslations()
  const { errorMessage, handleError, clearError } = useErrorHandler()
  const [submitted, setSubmitted] = useState<ContactFormValues | null>(null)

  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
  } = useForm<ContactFormValues>({
    resolver: zodResolver(contactSchema),
    defaultValues: {
      name: '',
      email: '',
      subject: '',
      message: '',
      priority: '',
    },
  })

  const onSubmit = async (data: ContactFormValues) => {
    clearError()
    try {
      // Simulate API call
      await new Promise((resolve) => setTimeout(resolve, 1000))
      setSubmitted(data)
      reset()
    } catch (err) {
      handleError(err)
    }
  }

  const subjectOptions = [
    { value: 'general', labelKey: 'form.subjectGeneral' },
    { value: 'support', labelKey: 'form.subjectSupport' },
    { value: 'billing', labelKey: 'form.subjectBilling' },
    { value: 'feedback', labelKey: 'form.subjectFeedback' },
  ]

  const priorityOptions = [
    { value: 'low', labelKey: 'form.priorityLow' },
    { value: 'medium', labelKey: 'form.priorityMedium' },
    { value: 'high', labelKey: 'form.priorityHigh' },
    { value: 'urgent', labelKey: 'form.priorityUrgent' },
  ]

  return (
    <div className="container mx-auto max-w-2xl py-10">
      <h1 className="mb-8 text-3xl font-bold">{t('form.exampleTitle')}</h1>

      {submitted && (
        <div className="mb-6 rounded-md border border-green-200 bg-green-50 p-4 text-green-800">
          <p className="font-medium">{t('form.submitted')}</p>
          <pre className="mt-2 text-sm">{JSON.stringify(submitted, null, 2)}</pre>
        </div>
      )}

      {errorMessage && (
        <div className="border-destructive/50 bg-destructive/10 text-destructive mb-6 rounded-md border p-4">
          {errorMessage}
        </div>
      )}

      <form onSubmit={handleSubmit(onSubmit)} className="space-y-6" noValidate>
        <FormField labelKey="form.name" error={errors.name?.message} required>
          {({ id, errorId }) => (
            <input
              {...register('name')}
              id={id}
              aria-describedby={errorId || undefined}
              aria-invalid={!!errors.name}
              className="border-input bg-background ring-offset-background placeholder:text-muted-foreground focus-visible:ring-ring flex h-10 w-full rounded-md border px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
              placeholder={t('form.namePlaceholder')}
            />
          )}
        </FormField>

        <FormField labelKey="form.email" error={errors.email?.message} required>
          {({ id, errorId }) => (
            <input
              {...register('email')}
              id={id}
              type="email"
              aria-describedby={errorId || undefined}
              aria-invalid={!!errors.email}
              className="border-input bg-background ring-offset-background placeholder:text-muted-foreground focus-visible:ring-ring flex h-10 w-full rounded-md border px-3 py-2 text-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
              placeholder={t('form.emailPlaceholder')}
            />
          )}
        </FormField>

        <SelectField
          labelKey="form.subject"
          options={subjectOptions}
          placeholderKey="form.selectSubject"
          error={errors.subject?.message}
          required
          {...register('subject')}
        />

        <SelectField
          labelKey="form.priority"
          options={priorityOptions}
          placeholderKey="form.selectPriority"
          error={errors.priority?.message}
          required
          {...register('priority')}
        />

        <TextAreaField
          labelKey="form.message"
          error={errors.message?.message}
          descriptionKey="form.messageDescription"
          required
          rows={5}
          placeholder={t('form.messagePlaceholder')}
          {...register('message')}
        />

        <div className="flex gap-4">
          <button
            type="submit"
            disabled={isSubmitting}
            className="bg-primary text-primary-foreground ring-offset-background hover:bg-primary/90 focus-visible:ring-ring inline-flex h-10 items-center justify-center rounded-md px-4 py-2 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50"
          >
            {isSubmitting ? t('form.submitting') : t('form.submit')}
          </button>

          <button
            type="button"
            onClick={() => {
              reset()
              clearError()
              setSubmitted(null)
            }}
            className="border-input bg-background ring-offset-background hover:bg-accent hover:text-accent-foreground focus-visible:ring-ring inline-flex h-10 items-center justify-center rounded-md border px-4 py-2 text-sm font-medium transition-colors focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
          >
            {t('form.reset')}
          </button>
        </div>
      </form>
    </div>
  )
}
